package cluster

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/code-ready/crc/pkg/crc/constants"
	"github.com/code-ready/crc/pkg/crc/errors"
	"github.com/code-ready/crc/pkg/crc/logging"
	"github.com/code-ready/crc/pkg/crc/network"
	"github.com/code-ready/crc/pkg/crc/oc"
	"github.com/code-ready/crc/pkg/crc/ssh"
	crctls "github.com/code-ready/crc/pkg/crc/tls"
	"github.com/code-ready/crc/pkg/crc/validation"
	"github.com/pborman/uuid"
)

// #nosec G101
const vmPullSecretPath = "/var/lib/kubelet/config.json"

const (
	KubeletServerCert = "/var/lib/kubelet/pki/kubelet-server-current.pem"
	KubeletClientCert = "/var/lib/kubelet/pki/kubelet-client-current.pem"

	AggregatorClientCert = "/etc/kubernetes/static-pod-resources/kube-apiserver-certs/configmaps/aggregator-client-ca/ca-bundle.crt"
)

func CheckCertsValidity(sshRunner *ssh.Runner) (map[string]bool, error) {
	statuses := make(map[string]bool)
	for _, cert := range []string{KubeletClientCert, KubeletServerCert, AggregatorClientCert} {
		expired, err := checkCertValidity(sshRunner, cert)
		if err != nil {
			return nil, err
		}
		statuses[cert] = expired
	}
	return statuses, nil
}

func checkCertValidity(sshRunner *ssh.Runner, cert string) (bool, error) {
	output, _, err := sshRunner.Run(fmt.Sprintf(`date --date="$(sudo openssl x509 -in %s -noout -enddate | cut -d= -f 2)" --iso-8601=seconds`, cert))
	if err != nil {
		return false, err
	}
	expiryDate, err := time.Parse(time.RFC3339, strings.TrimSpace(output))
	if err != nil {
		return false, err
	}
	if time.Now().After(expiryDate) {
		logging.Debugf("Certs have expired, they were valid till: %s", expiryDate.Format(time.RFC822))
		return true, nil
	}
	return false, nil
}

// Return size of disk, used space in bytes and the mountpoint
func GetRootPartitionUsage(sshRunner *ssh.Runner) (int64, int64, error) {
	cmd := "df -B1 --output=size,used,target /sysroot | tail -1"

	out, _, err := sshRunner.Run(cmd)

	if err != nil {
		return 0, 0, err
	}
	diskDetails := strings.Split(strings.TrimSpace(out), " ")
	diskSize, err := strconv.ParseInt(diskDetails[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	diskUsage, err := strconv.ParseInt(diskDetails[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return diskSize, diskUsage, nil
}

func EnsureSSHKeyPresentInTheCluster(ctx context.Context, ocConfig oc.Config, sshPublicKeyPath string) error {
	sshPublicKeyByte, err := ioutil.ReadFile(sshPublicKeyPath)
	if err != nil {
		return err
	}
	sshPublicKey := strings.TrimRight(string(sshPublicKeyByte), "\r\n")
	if err := WaitForOpenshiftResource(ctx, ocConfig, "machineconfigs"); err != nil {
		return err
	}
	stdout, stderr, err := ocConfig.RunOcCommand("get", "machineconfigs", "99-master-ssh", "-o", `jsonpath='{.spec.config.passwd.users[0].sshAuthorizedKeys[0]}'`)
	if err != nil {
		return fmt.Errorf("Failed to get machine configs %v: %s", err, stderr)
	}
	if stdout == string(sshPublicKey) {
		return nil
	}
	logging.Info("Updating SSH key to machine config resource...")
	cmdArgs := []string{"patch", "machineconfig", "99-master-ssh", "-p",
		fmt.Sprintf(`'{"spec": {"config": {"passwd": {"users": [{"name": "core", "sshAuthorizedKeys": ["%s"]}]}}}}'`, sshPublicKey),
		"--type", "merge"}
	_, stderr, err = ocConfig.RunOcCommand(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to update ssh key %v: %s", err, stderr)
	}
	return nil
}

func EnsurePullSecretPresentInTheCluster(ctx context.Context, ocConfig oc.Config, pullSec PullSecretLoader) error {
	if err := WaitForOpenshiftResource(ctx, ocConfig, "secret"); err != nil {
		return err
	}

	stdout, stderr, err := ocConfig.RunOcCommandPrivate("get", "secret", "pull-secret", "-n", "openshift-config", "-o", `jsonpath="{['data']['\.dockerconfigjson']}"`)
	if err != nil {
		return fmt.Errorf("Failed to get pull secret %v: %s", err, stderr)
	}
	decoded, err := base64.StdEncoding.DecodeString(stdout)
	if err != nil {
		return err
	}
	if err := validation.ImagePullSecret(string(decoded)); err == nil {
		return nil
	}

	logging.Info("Adding user's pull secret to the cluster...")
	content, err := pullSec.Value()
	if err != nil {
		return err
	}
	base64OfPullSec := base64.StdEncoding.EncodeToString([]byte(content))
	cmdArgs := []string{"patch", "secret", "pull-secret", "-p",
		fmt.Sprintf(`'{"data":{".dockerconfigjson":"%s"}}'`, base64OfPullSec),
		"-n", "openshift-config", "--type", "merge"}

	_, stderr, err = ocConfig.RunOcCommandPrivate(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to add Pull secret %v: %s", err, stderr)
	}
	return nil
}

func EnsureGeneratedClientCAPresentInTheCluster(ctx context.Context, ocConfig oc.Config, sshRunner *ssh.Runner, selfSignedCACert *x509.Certificate, adminCert string) error {
	selfSignedCAPem := crctls.CertToPem(selfSignedCACert)
	if err := WaitForOpenshiftResource(ctx, ocConfig, "configmaps"); err != nil {
		return err
	}
	clusterClientCA, stderr, err := ocConfig.RunOcCommand("get", "configmaps", "admin-kubeconfig-client-ca", "-n", "openshift-config", "-o", `jsonpath="{.data.ca-bundle\.crt}"`)
	if err != nil {
		return fmt.Errorf("Failed to get config map %v: %s", err, stderr)
	}

	ok, err := crctls.VerifyCertificateAgainstRootCA(clusterClientCA, adminCert)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	logging.Info("Updating root CA cert to admin-kubeconfig-client-ca configmap...")
	jsonPath := fmt.Sprintf(`'{"data": {"ca-bundle.crt": %q}}'`, selfSignedCAPem)
	cmdArgs := []string{"patch", "configmap", "admin-kubeconfig-client-ca",
		"-n", "openshift-config", "--patch", jsonPath}
	_, stderr, err = ocConfig.RunOcCommand(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to patch admin-kubeconfig-client-ca config map with new CA` %v: %s", err, stderr)
	}
	if err := sshRunner.CopyFile(constants.KubeconfigFilePath, ocConfig.KubeconfigPath, 0644); err != nil {
		return fmt.Errorf("Failed to copy generated kubeconfig file to VM: %v", err)
	}

	return nil
}

func RemovePullSecretFromCluster(ctx context.Context, ocConfig oc.Config, sshRunner *ssh.Runner) error {
	logging.Info("Removing user's pull secret from instance disk and from cluster secret...")
	cmdArgs := []string{"patch", "secret", "pull-secret", "-p",
		`'{"data":{".dockerconfigjson":"e30K"}}'`,
		"-n", "openshift-config", "--type", "merge"}

	_, stderr, err := ocConfig.RunOcCommand(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to remove Pull secret %w: %s", err, stderr)
	}
	return waitForPullSecretRemovedFromInstanceDisk(ctx, sshRunner)
}

func waitForPullSecretRemovedFromInstanceDisk(ctx context.Context, sshRunner *ssh.Runner) error {
	logging.Info("Waiting for user's pull secret removed from instance disk...")
	pullSecretPresentFunc := func() error {
		stdout, stderr, err := sshRunner.RunPrivate(fmt.Sprintf("sudo cat %s", vmPullSecretPath))
		if err != nil {
			return &errors.RetriableError{Err: fmt.Errorf("failed to read %s file: %v: %s", vmPullSecretPath, err, stderr)}
		}
		if err := validation.ImagePullSecret(stdout); err == nil {
			return &errors.RetriableError{Err: fmt.Errorf("pull secret still part of instance disk")}
		}
		return nil
	}
	return errors.Retry(ctx, 1*time.Minute, pullSecretPresentFunc, 2*time.Second)
}

func RemoveOldRenderedMachineConfig(ocConfig oc.Config) error {
	// This block (#L179-183) should be removed as soon as we start shipping with 4.8 bundle.
	// This check is only make sure if there is any machineconfig resource or not because
	// in 4.7 we disabled mco and also deleted all the machineconfig/machineconfig-pools.
	// For 4.8 we don't disable mco and it does contain the machineconfigs.
	stdout, stderr, err := ocConfig.RunOcCommand("get mc --sort-by=.metadata.creationTimestamp --no-headers -oname")
	if err != nil {
		return fmt.Errorf("failed to get machineconfig resource %w: %s", err, stderr)
	}
	sortedMachineConfigsWithTime := strings.Split(stdout, "\n")
	if len(sortedMachineConfigsWithTime) == 0 {
		return nil
	}

	// We need to make sure only old machine configs are deleted not the new one.
	var (
		renderedMaster []string
		renderedWorker []string
	)
	for _, mc := range sortedMachineConfigsWithTime {
		if strings.Contains(mc, "rendered-master") {
			renderedMaster = append(renderedMaster, mc)
		}
		if strings.Contains(mc, "rendered-worker") {
			renderedWorker = append(renderedWorker, mc)
		}
	}

	var deleteRenderedMachineConfig string
	if len(renderedMaster) > 0 {
		deleteRenderedMachineConfig = strings.Join(renderedMaster[:len(renderedMaster)-1], " ")
	}
	if len(renderedWorker) > 0 {
		deleteRenderedMachineConfig = fmt.Sprintf("%s %s", deleteRenderedMachineConfig, strings.Join(renderedWorker[:len(renderedWorker)-1], " "))
	}

	if deleteRenderedMachineConfig != "" {
		_, stderr, err = ocConfig.RunOcCommand(fmt.Sprintf("delete %s", deleteRenderedMachineConfig))
		if err != nil {
			return fmt.Errorf("Failed to remove machineconfigpools %w: %s", err, stderr)
		}
	}
	return nil
}

func EnsureClusterIDIsNotEmpty(ctx context.Context, ocConfig oc.Config) error {
	if err := WaitForOpenshiftResource(ctx, ocConfig, "clusterversion"); err != nil {
		return err
	}

	stdout, stderr, err := ocConfig.RunOcCommand("get", "clusterversion", "version", "-o", `jsonpath="{['spec']['clusterID']}"`)
	if err != nil {
		return fmt.Errorf("Failed to get clusterversion %v: %s", err, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		return nil
	}

	logging.Info("Updating cluster ID...")
	clusterID := uuid.New()
	cmdArgs := []string{"patch", "clusterversion", "version", "-p",
		fmt.Sprintf(`'{"spec":{"clusterID":"%s"}}'`, clusterID), "--type", "merge"}

	_, stderr, err = ocConfig.RunOcCommand(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to update cluster ID %v: %s", err, stderr)
	}

	return nil
}

func AddProxyConfigToCluster(ctx context.Context, sshRunner *ssh.Runner, ocConfig oc.Config, proxy *network.ProxyConfig) error {
	type trustedCA struct {
		Name string `json:"name"`
	}

	type proxySpecConfig struct {
		HTTPProxy  string    `json:"httpProxy"`
		HTTPSProxy string    `json:"httpsProxy"`
		NoProxy    string    `json:"noProxy"`
		TrustedCA  trustedCA `json:"trustedCA"`
	}

	type patchSpec struct {
		Spec proxySpecConfig `json:"spec"`
	}

	patch := &patchSpec{
		Spec: proxySpecConfig{
			HTTPProxy:  proxy.HTTPProxy,
			HTTPSProxy: proxy.HTTPSProxy,
			NoProxy:    proxy.GetNoProxyString(),
		},
	}

	if err := WaitForOpenshiftResource(ctx, ocConfig, "proxy"); err != nil {
		return err
	}

	if proxy.ProxyCACert != "" {
		trustedCAName := "user-ca-bundle"
		logging.Debug("Adding proxy CA cert to cluster")
		if err := addProxyCACertToCluster(sshRunner, ocConfig, proxy, trustedCAName); err != nil {
			return err
		}
		patch.Spec.TrustedCA = trustedCA{Name: trustedCAName}
	}

	patchEncode, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("Failed to encode to json: %v", err)
	}
	logging.Debugf("Patch string %s", string(patchEncode))

	cmdArgs := []string{"patch", "proxy", "cluster", "-p", fmt.Sprintf("'%s'", string(patchEncode)), "-n", "openshift-config", "--type", "merge"}
	if _, stderr, err := ocConfig.RunOcCommandPrivate(cmdArgs...); err != nil {
		return fmt.Errorf("Failed to add proxy details %v: %s", err, stderr)
	}
	return nil
}

func addProxyCACertToCluster(sshRunner *ssh.Runner, ocConfig oc.Config, proxy *network.ProxyConfig, trustedCAName string) error {
	proxyConfigMapFileName := fmt.Sprintf("/tmp/%s.json", trustedCAName)
	proxyCABundleTemplate := `{
  "apiVersion": "v1",
  "data": {
    "ca-bundle.crt": "%s"
  },
  "kind": "ConfigMap",
  "metadata": {
    "name": "%s",
    "namespace": "openshift-config"
  }
}
`
	// Replace the carriage return ("\n" or "\r\n") with literal `\n` string
	re := regexp.MustCompile(`\r?\n`)
	p := fmt.Sprintf(proxyCABundleTemplate, re.ReplaceAllString(proxy.ProxyCACert, `\n`), trustedCAName)
	err := sshRunner.CopyData([]byte(p), proxyConfigMapFileName, 0644)
	if err != nil {
		return err
	}
	cmdArgs := []string{"apply", "-f", proxyConfigMapFileName}
	if _, stderr, err := ocConfig.RunOcCommandPrivate(cmdArgs...); err != nil {
		return fmt.Errorf("Failed to add proxy cert details %v: %s", err, stderr)
	}
	return nil
}

type PullSecretMemoizer struct {
	value  string
	Getter PullSecretLoader
}

func (p *PullSecretMemoizer) Value() (string, error) {
	if p.value != "" {
		return p.value, nil
	}
	val, err := p.Getter.Value()
	if err == nil {
		p.value = val
	}
	return val, err
}

func EnsurePullSecretPresentOnInstanceDisk(sshRunner *ssh.Runner, pullSecret PullSecretLoader) error {
	if _, _, err := sshRunner.Run(fmt.Sprintf("test -e %s", vmPullSecretPath)); err == nil {
		return nil
	}
	logging.Info("Adding user's pull secret to instance disk...")
	content, err := pullSecret.Value()
	if err != nil {
		return err
	}
	return sshRunner.CopyData([]byte(content), vmPullSecretPath, 0600)
}

func WaitForPullSecretPresentOnInstanceDisk(ctx context.Context, sshRunner *ssh.Runner) error {
	logging.Info("Waiting for user's pull secret part of instance disk...")
	pullSecretPresentFunc := func() error {
		stdout, stderr, err := sshRunner.RunPrivate(fmt.Sprintf("sudo cat %s", vmPullSecretPath))
		if err != nil {
			return fmt.Errorf("failed to read %s file: %v: %s", vmPullSecretPath, err, stderr)
		}
		if err := validation.ImagePullSecret(stdout); err != nil {
			return &errors.RetriableError{Err: fmt.Errorf("pull secret not updated to disk")}
		}
		return nil
	}
	return errors.Retry(ctx, 7*time.Minute, pullSecretPresentFunc, 2*time.Second)
}

func WaitForRequestHeaderClientCaFile(ctx context.Context, sshRunner *ssh.Runner) error {
	lookupRequestHeaderClientCa := func() error {
		expired, err := checkCertValidity(sshRunner, AggregatorClientCert)
		if err != nil {
			return fmt.Errorf("Failed to the expiry date: %v", err)
		}
		if expired {
			return &errors.RetriableError{Err: fmt.Errorf("certificate still expired")}
		}
		return nil
	}
	return errors.Retry(ctx, 8*time.Minute, lookupRequestHeaderClientCa, 2*time.Second)
}

func WaitForAPIServer(ctx context.Context, ocConfig oc.Config) error {
	logging.Info("Waiting for kube-apiserver availability... [takes around 2min]")
	waitForAPIServer := func() error {
		stdout, stderr, err := ocConfig.WithFailFast().RunOcCommand("get", "nodes")
		if err != nil {
			logging.Debug(stderr)
			return &errors.RetriableError{Err: err}
		}
		logging.Debug(stdout)
		return nil
	}
	return errors.Retry(ctx, 4*time.Minute, waitForAPIServer, time.Second)
}

func DeleteOpenshiftAPIServerPods(ctx context.Context, ocConfig oc.Config) error {
	if err := WaitForOpenshiftResource(ctx, ocConfig, "pod"); err != nil {
		return err
	}

	deleteOpenshiftAPIServerPods := func() error {
		cmdArgs := []string{"delete", "pod", "--all", "--force", "-n", "openshift-apiserver"}
		_, stderr, err := ocConfig.WithFailFast().RunOcCommand(cmdArgs...)
		if err != nil {
			return &errors.RetriableError{Err: fmt.Errorf("Failed to delete pod from openshift-apiserver namespace %v: %s", err, stderr)}
		}
		return nil
	}

	return errors.Retry(ctx, 60*time.Second, deleteOpenshiftAPIServerPods, time.Second)
}

func CheckProxySettingsForOperator(ocConfig oc.Config, proxy *network.ProxyConfig, deployment, namespace string) (bool, error) {
	if !proxy.IsEnabled() {
		logging.Debugf("No proxy in use")
		return true, nil
	}
	cmdArgs := []string{"set", "env", "deployment", deployment, "--list", "-n", namespace}
	out, _, err := ocConfig.RunOcCommandPrivate(cmdArgs...)
	if err != nil {
		return false, err
	}
	if strings.Contains(out, proxy.HTTPSProxy) || strings.Contains(out, proxy.HTTPProxy) {
		return true, nil
	}
	return false, nil
}

func DeleteMCOLeaderLease(ctx context.Context, ocConfig oc.Config) error {
	if err := WaitForOpenshiftResource(ctx, ocConfig, "cm"); err != nil {
		return err
	}
	_, _, err := ocConfig.RunOcCommand("delete", "-n", "openshift-machine-config-operator", "cm", "machine-config-controller")
	return err
}
