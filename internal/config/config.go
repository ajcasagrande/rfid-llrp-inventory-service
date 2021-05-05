//
// Copyright (C) 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"github.com/edgexfoundry/go-mod-bootstrap/v2/bootstrap/config"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/clients/logger"
	"github.com/pkg/errors"
	"strconv"
	"time"
)

type ServiceConfig struct {
	AppCustom AppCustomConfig
}

// AppCustomConfig is service's custom structured configuration that is specified in the service's
// configuration.toml file and Configuration Provider (aka Consul), if enabled.
type AppCustomConfig struct {
	MobilityProfileThreshold     float64
	MobilityProfileHoldoffMillis float64
	MobilityProfileSlope         float64

	DeviceServiceName  string
	DeviceServiceURL   string
	MetadataServiceURL string

	DepartedThresholdSeconds     uint
	DepartedCheckIntervalSeconds uint
	AgeOutHours                  uint

	AdjustLastReadOnByOrigin bool

	Aliases map[string]string
}

var (
	// ErrUnexpectedConfigItems is returned when the input configuration map has extra keys
	// and values that are left over after parsing is complete
	ErrUnexpectedConfigItems = errors.New("unexpected config items")
	// ErrMissingRequiredKey is returned when we are unable to parse the value for a config key
	ErrMissingRequiredKey = errors.New("missing required key")
	// ErrOutOfRange is returned if a config value is syntactically valid for its type,
	// but otherwise outside of the acceptable range of valid values.
	ErrOutOfRange = errors.New("config value out of range")
)

// NewServiceConfig returns a new ServiceConfig instance with default values.
func NewServiceConfig() ServiceConfig {
	return ServiceConfig{
		AppCustom: AppCustomConfig{
			MobilityProfileThreshold:     6,
			MobilityProfileHoldoffMillis: 500,
			MobilityProfileSlope:         -0.008,
			DeviceServiceName:            "edgex-device-llrp",
			DeviceServiceURL:             "http://edgex-device-llrp:51992/",
			MetadataServiceURL:           "http://edgex-core-metadata:48081/",
			DepartedThresholdSeconds:     600,
			DepartedCheckIntervalSeconds: 30,
			AgeOutHours:                  336,
			AdjustLastReadOnByOrigin:     true,
			Aliases: map[string]string{},
		},
	}
}

// Validate returns nil if the ApplicationSettings are valid,
// or the first validation error it encounters.
func (ac AppCustomConfig) Validate() error {
	if ac.DepartedThresholdSeconds == 0 {
		return errors.Wrap(ErrOutOfRange, "DepartedThresholdSeconds must be >0")
	}

	if ac.DepartedCheckIntervalSeconds == 0 {
		return errors.Wrap(ErrOutOfRange, "DepartedCheckIntervalSeconds must be >0")
	}

	if ac.AgeOutHours == 0 {
		return errors.Wrap(ErrOutOfRange, "AgeOutHours must be >0")
	}

	return nil
}

// ParseServiceConfig returns a new ServiceConfig
// with settings parsed from the given map,
// merged with default settings for missing value.
//
// It returns a parsing error if a given key's value cannot be parsed,
// an error wrapping ErrMissingRequiredKey if a required key is missing,
// and an error wrapping ErrUnexpectedConfigItems if the map has unknown config keys.
//
// If the map is missing a non-required key,
// it logs an INFO message unless the given logging client is nil.
func ParseServiceConfig(lc logger.LoggingClient, configMap map[string]string) (ServiceConfig, error) {
	cfg := NewServiceConfig()
	settings := &cfg.AppCustom

	type confItem struct {
		target   interface{} // pointer to the variable to set
		required bool        // if true, return an error if not in the map
	}

	used := make(map[string]bool, len(configMap))
	for key, ci := range map[string]confItem{
		"AdjustLastReadOnByOrigin":     {target: &settings.AdjustLastReadOnByOrigin},
		"DepartedThresholdSeconds":     {target: &settings.DepartedThresholdSeconds},
		"DepartedCheckIntervalSeconds": {target: &settings.DepartedCheckIntervalSeconds},
		"AgeOutHours":                  {target: &settings.AgeOutHours},
		"MobilityProfileThreshold":     {target: &settings.MobilityProfileThreshold},
		"MobilityProfileHoldoffMillis": {target: &settings.MobilityProfileHoldoffMillis},
		"MobilityProfileSlope":         {target: &settings.MobilityProfileSlope},
		"DeviceServiceName":            {target: &settings.DeviceServiceName},
		"DeviceServiceURL":             {target: &settings.DeviceServiceURL},
		"MetadataServiceURL":           {target: &settings.MetadataServiceURL},
	} {
		var err error

		val, ok := configMap[key]
		if !ok {
			if ci.required {
				return cfg, errors.Wrapf(ErrMissingRequiredKey, "no value for %q", key)
			}

			if lc != nil {
				lc.Info("Using default value for config item.",
					"key", key, "value", ci.target)
			}
			continue
		}

		switch target := ci.target.(type) {
		default:
			panic(fmt.Sprintf("unhandled type for config item %q: %T",
				key, ci.target))

		case *string:
			*target = val
		case *float64:
			*target, err = strconv.ParseFloat(val, 64)
		case *bool:
			*target, err = strconv.ParseBool(val)
		case *int:
			*target, err = strconv.Atoi(val)

		case *uint:
			u, perr := strconv.ParseUint(val, 10, 0)
			err = perr
			*target = uint(u)
		}

		if err != nil {
			return cfg, errors.Wrapf(err, "failed to parse config item %q, %q", key, val)
		}

		used[key] = true
	}

	if err := settings.Validate(); err != nil {
		return cfg, err
	}

	var missed []string
	for key, val := range configMap {
		if !used[key] {
			missed = append(missed, fmt.Sprintf("%q: %q", key, val))
		}
	}

	if len(missed) != 0 {
		return cfg, errors.Wrapf(ErrUnexpectedConfigItems, "unused config items: %s", missed)
	}

	return cfg, nil
}


// TODO: Update using your Custom configuration type.
// UpdateFromRaw updates the service's full configuration from raw data received from
// the Service Provider.
func (c *ServiceConfig) UpdateFromRaw(rawConfig interface{}) bool {
	configuration, ok := rawConfig.(*ServiceConfig)
	if !ok {
		return false //errors.New("unable to cast raw config to type 'ServiceConfig'")
	}

	newConfig, ok := rawConfig.(*config.ServiceConfig)
	if !ok {
		lc.Warn("Unable to decode configuration from consul.", "raw", fmt.Sprintf("%#v", rawConfig))
		return false
	}

	if err := newconfig.AppCustom.Validate(); err != nil {
		lc.Error("Invalid Consul configuration.", "error", err.Error())
		return false
	}

	lc.Info("Configuration updated from consul.")
	lc.Debug("New consul config.", "config", fmt.Sprintf("%+v", newConfig))
	processor.UpdateConfig(*newConfig)

	// check if we need to change the ticker interval
	if departedCheckSeconds != newconfig.AppCustom.DepartedCheckIntervalSeconds {
		aggregateDepartedTicker.Stop()
		departedCheckSeconds = newconfig.AppCustom.DepartedCheckIntervalSeconds
		aggregateDepartedTicker = time.NewTicker(time.Duration(departedCheckSeconds) * time.Second)
		lc.Info(fmt.Sprintf("Changing aggregate departed check interval to %d seconds.", departedCheckSeconds))
	}

	*c = *configuration

	return true
}
