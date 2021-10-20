//
// Copyright (C) 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"github.com/pkg/errors"
)

// ApplicationSettings is a struct that defines the ApplicationSettings section of the
// configuration.toml file.
type ApplicationSettings struct {
	MobilityProfileThreshold     float64
	MobilityProfileHoldoffMillis float64
	MobilityProfileSlope         float64

	DeviceServiceName string
	DeviceServiceURL  string

	DepartedThresholdSeconds     uint
	DepartedCheckIntervalSeconds uint
	AgeOutHours                  uint

	AdjustLastReadOnByOrigin bool
}

// CustomConfig is the struct representation of all of the sections from the configuration.toml
// that we are interested in syncing with Consul.
type CustomConfig struct {
	AppSettings ApplicationSettings
	Aliases     map[string]string
}

type ServiceConfig struct {
	AppCustom CustomConfig
}

// UpdateFromRaw updates the service's full configuration from raw data received from
// the Service Provider.
func (c *ServiceConfig) UpdateFromRaw(rawConfig interface{}) bool {
	configuration, ok := rawConfig.(*ServiceConfig)
	if !ok {
		return false //errors.New("unable to cast raw config to type 'ServiceConfig'")
	}

	*c = *configuration

	return true
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

// NeServiceConfig returns a new ServiceConfig instance with default values.
func NewServiceConfig() ServiceConfig {
	return ServiceConfig{
		AppCustom: CustomConfig{
			Aliases: map[string]string{},
			AppSettings: ApplicationSettings{
				MobilityProfileThreshold:     6,
				MobilityProfileHoldoffMillis: 500,
				MobilityProfileSlope:         -0.008,
				DeviceServiceName:            "edgex-device-llrp",
				DeviceServiceURL:             "http://edgex-device-llrp:49989/",
				DepartedThresholdSeconds:     600,
				DepartedCheckIntervalSeconds: 30,
				AgeOutHours:                  336,
				AdjustLastReadOnByOrigin:     true,
			},
		},
	}
}

// Validate returns nil if the ApplicationSettings are valid,
// or the first validation error it encounters.
func (as ApplicationSettings) Validate() error {
	if as.DepartedThresholdSeconds == 0 {
		return errors.Wrap(ErrOutOfRange, "DepartedThresholdSeconds must be >0")
	}

	if as.DepartedCheckIntervalSeconds == 0 {
		return errors.Wrap(ErrOutOfRange, "DepartedCheckIntervalSeconds must be >0")
	}

	if as.AgeOutHours == 0 {
		return errors.Wrap(ErrOutOfRange, "AgeOutHours must be >0")
	}

	return nil
}
