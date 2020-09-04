package config

import (
	"fmt"
	"reflect"

	"github.com/spf13/cast"
)

const (
	configPropDoesntExistMsg = "Configuration property '%s' does not exist"
)

type Config struct {
	storage        RawStorage
	settingsByName map[string]Setting
}

func New(storage RawStorage) *Config {
	return &Config{
		storage:        storage,
		settingsByName: make(map[string]Setting),
	}
}

// AllConfigs returns all the known configs
// A known config is one which was registered through AddSetting
// - config with a default value
// - config with a value set
// - config with no value set
func (c *Config) AllConfigs() map[string]SettingValue {
	var allConfigs = make(map[string]SettingValue)
	for key := range c.settingsByName {
		allConfigs[key] = c.Get(key)
	}
	return allConfigs
}

// AddSetting returns a filled struct of ConfigSetting
// takes the config name and default value as arguments
func (c *Config) AddSetting(name string, defValue interface{}, validationFn ValidationFnType, callbackFn SetFn) {
	c.settingsByName[name] = Setting{
		Name:         name,
		defaultValue: defValue,
		validationFn: validationFn,
		callbackFn:   callbackFn,
	}
}

// Set sets the value for a given config key
func (c *Config) Set(key string, value interface{}) (string, error) {
	setting, ok := c.settingsByName[key]
	if !ok {
		return "", fmt.Errorf(configPropDoesntExistMsg, key)
	}

	var castValue interface{}
	switch setting.defaultValue.(type) {
	case int:
		castValue = cast.ToInt(value)
	case string:
		castValue = cast.ToString(value)
	case bool:
		castValue = cast.ToBool(value)
	}

	ok, expectedValue := c.settingsByName[key].validationFn(castValue)
	if !ok {
		return "", fmt.Errorf("Value '%s' for configuration property '%s' is invalid, reason: %s", castValue, key, expectedValue)
	}

	if err := c.storage.Set(key, castValue); err != nil {
		return "", err
	}

	return c.settingsByName[key].callbackFn(key, castValue), nil
}

// Unset unsets a given config key
func (c *Config) Unset(key string) (string, error) {
	_, ok := c.settingsByName[key]
	if !ok {
		return "", fmt.Errorf(configPropDoesntExistMsg, key)
	}

	if err := c.storage.Unset(key); err != nil {
		return "", err
	}

	return fmt.Sprintf("Successfully unset configuration property '%s'", key), nil
}

func (c *Config) Get(key string) SettingValue {
	setting, ok := c.settingsByName[key]
	if !ok {
		return SettingValue{
			Invalid: true,
		}
	}
	value := c.storage.Get(key)
	if value == nil {
		value = setting.defaultValue
	}
	switch setting.defaultValue.(type) {
	case int:
		value = cast.ToInt(value)
	case string:
		value = cast.ToString(value)
	case bool:
		value = cast.ToBool(value)
	default:
		return SettingValue{
			Invalid: true,
		}
	}
	return SettingValue{
		Value:     value,
		IsDefault: reflect.DeepEqual(setting.defaultValue, value),
	}
}