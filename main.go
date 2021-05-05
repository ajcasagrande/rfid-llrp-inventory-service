//
// Copyright (C) 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"edgexfoundry-holding/rfid-llrp-inventory-service/internal/config"

	"context"
	"edgexfoundry-holding/rfid-llrp-inventory-service/functions"
	"edgexfoundry-holding/rfid-llrp-inventory-service/internal/inventory"
	"edgexfoundry-holding/rfid-llrp-inventory-service/internal/llrp"
	"encoding/json"
	"fmt"
	"github.com/edgexfoundry/app-functions-sdk-go/v2/pkg"
	"github.com/edgexfoundry/app-functions-sdk-go/v2/pkg/interfaces"
	"github.com/edgexfoundry/app-functions-sdk-go/v2/pkg/transforms"
	"github.com/edgexfoundry/go-mod-bootstrap/v2/bootstrap/flags"
	"github.com/edgexfoundry/go-mod-configuration/v2/configuration"
	"github.com/edgexfoundry/go-mod-configuration/v2/pkg/types"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/clients/logger"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/models"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	serviceKey      = "rfid-llrp-inventory"
	eventDeviceName = "rfid-llrp-inventory"

	BaseConsulPath = "edgex/appservices/1.0/"

	ResourceROAccessReport     = "ROAccessReport"
	ResourceReaderNotification = "ReaderEventNotification"
	ResourceInventoryEvent     = "InventoryEvent"

	maxBodyBytes        = 100 * 1024
	coreDataPostTimeout = 3 * time.Minute
	eventChSz           = 100

	cacheFolder  = "cache"
	tagCacheFile = "tags.json"
	folderPerm   = 0755 // folders require the execute flag in order to create new files
	filePerm     = 0644
)

type inventoryApp struct {
	service       interfaces.ApplicationService
	serviceConfig *config.ServiceConfig
	configChanged chan bool

	lgr          logWrap
	devMu        sync.RWMutex
	devService   llrp.DSClient
	defaultGrp   *llrp.ReaderGroup
	snapshotReqs chan snapshotDest
	reports      chan reportData
}

type reportData struct {
	report *llrp.ROAccessReport
	info   inventory.ReportInfo
}

type snapshotDest struct {
	w      io.Writer
	result chan error
}

type logWrap struct {
	logger.LoggingClient
}

type lg struct {
	key string
	val interface{}
}

func (lgr logWrap) errIf(cond bool, msg string, params ...lg) bool {
	if !cond {
		return false
	}

	if len(params) > 0 {
		parts := make([]interface{}, len(params)*2)
		for i := range params {
			parts[i*2] = params[i].key
			parts[i*2+1] = params[i].val
		}
		lgr.Error(msg, parts...)
	} else {
		lgr.Error(msg)
	}

	return true
}

func (lgr logWrap) exitIf(cond bool, msg string, params ...lg) {
	if lgr.errIf(cond, msg, params...) {
		os.Exit(1)
	}
}

func (lgr logWrap) exitIfErr(err error, msg string, params ...lg) {
	lgr.exitIf(err != nil, msg, append(params, lg{"error", err})...)
}

func main() {
	// TODO: See https://docs.edgexfoundry.org/2.0/microservices/application/ApplicationServices/
	//       for documentation on application services.
	app := inventoryApp{
		snapshotReqs: make(chan snapshotDest),
		reports:      make(chan reportData),
	}
	code := app.CreateAndRunAppService(serviceKey, pkg.NewAppService)
	os.Exit(code)
}



// TODO: Update using your Custom configuration 'writeable' type or remove if not using ListenForCustomConfigChanges
// ProcessConfigUpdates processes the updated configuration for the service's writable configuration.
// At a minimum it must copy the updated configuration into the service's current configuration. Then it can
// do any special processing for changes that require more.
func (app *inventoryApp) ProcessConfigUpdates(rawWritableConfig interface{}) {
	updated, ok := rawWritableConfig.(*config.AppCustomConfig)
	if !ok {
		app.lgr.Error("unable to process config updates: Can not cast raw config to type 'AppCustomConfig'")
		return
	}

	previous := app.serviceConfig.AppCustom
	app.serviceConfig.AppCustom = *updated

	if reflect.DeepEqual(previous, updated) {
		app.lgr.Info("No changes detected")
		return
	}
}

// CreateAndRunAppService wraps what would normally be in main() so that it can be unit tested
// TODO: Remove and just use regular main() if unit tests of main logic not needed.
func (app *inventoryApp) CreateAndRunAppService(serviceKey string, newServiceFactory func(string) (interfaces.ApplicationService, bool)) int {
	var ok bool
	app.service, ok = newServiceFactory(serviceKey)
	if !ok {
		return -1
	}

	app.lgr = logWrap{app.service.LoggingClient()}
	app.lgr.Info("Starting.")

	appSettings := app.service.ApplicationSettings()
	app.lgr.exitIf(appSettings == nil, "Missing application settings.")


	cfg, err := config.ParseServiceConfig(app.service.LoggingClient(), app.service.ApplicationSettings())
	app.lgr.exitIf(err != nil && !errors.Is(err, config.ErrUnexpectedConfigItems), fmt.Sprintf("Config parse error: %v.", err))
	app.serviceConfig = &cfg

	metadataURI, err := url.Parse(strings.TrimSpace(cfg.AppCustom.MetadataServiceURL))
	app.lgr.exitIfErr(err, "Invalid device service URL.")
	app.lgr.exitIf(metadataURI.Scheme == "" || metadataURI.Host == "",
		"Invalid metadata service URL.", lg{"endpoint", metadataURI.String()})

	devServURI, err := url.Parse(strings.TrimSpace(cfg.AppCustom.DeviceServiceURL))
	app.lgr.exitIfErr(err, "Invalid device service URL.")
	app.lgr.exitIf(devServURI.Scheme == "" || devServURI.Host == "",
		"Invalid device service URL.", lg{"endpoint", devServURI.String()})

	app.defaultGrp = llrp.NewReaderGroup()
	app.devService = llrp.NewDSClient(&url.URL{
		Scheme: devServURI.Scheme,
		Host:   devServURI.Host,
	}, http.DefaultClient)

	dsName := cfg.AppCustom.DeviceServiceName
	app.lgr.exitIf(dsName == "", "Missing device service name.")
	metadataURI.Path = "/api/v1/device/servicename/" + dsName
	deviceNames, err := llrp.GetDevices(metadataURI.String(), http.DefaultClient)
	app.lgr.exitIfErr(err, "Failed to get existing device names.", lg{"path", metadataURI.String()})
	for _, name := range deviceNames {
		app.lgr.exitIfErr(app.defaultGrp.AddReader(app.devService, name),
			"Failed to setup device.", lg{"device", name})
	}

	// More advance custom structured configuration can be defined and loaded as in this example.
	// For more details see https://docs.edgexfoundry.org/2.0/microservices/application/GeneralAppServiceConfig/#custom-configuration
	// TODO: Change to use your service's custom configuration struct
	//       or remove if not using custom configuration capability
	app.serviceConfig = &config.ServiceConfig{}
	if err := app.service.LoadCustomConfig(app.serviceConfig, "AppCustom"); err != nil {
		app.lgr.Errorf("failed load custom configuration: %s", err.Error())
		return -1
	}

	// Optionally validate the custom configuration after it is loaded.
	// TODO: remove if you don't have custom configuration or don't need to validate it
	if err := app.serviceConfig.AppCustom.Validate(); err != nil {
		app.lgr.Errorf("custom configuration failed validation: %s", err.Error())
		return -1
	}

	app.addRoutes()

	if err := os.MkdirAll(cacheFolder, folderPerm); err != nil {
		app.lgr.Error("Failed to create cache directory.", "directory", cacheFolder, "error", err.Error())
	}

	// Custom configuration can be 'writable' or a section of the configuration can be 'writable' when using
	// the Configuration Provider, aka Consul.
	// For more details see https://docs.edgexfoundry.org/2.0/microservices/application/GeneralAppServiceConfig/#writable-custom-configuration
	// TODO: Remove if not using writable custom configuration
	if err := app.service.ListenForCustomConfigChanges(&app.serviceConfig.AppCustom, "AppCustom", app.ProcessConfigUpdates); err != nil {
		app.lgr.Errorf("unable to watch custom writable configuration: %s", err.Error())
		return -1
	}

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.taskLoop(ctx, cfg, app.lgr)
		app.lgr.Info("Task loop has exited.")
	}()

	// We are doing this because of an issue with running app-functions-sdk inside
	// of docker-compose where something is hanging and not relinquishing control
	// back to our code.
	//
	// Note that this code does not in any way attempt to "fix" the deadlock issue,
	// but instead provides our code a way to cleanup and persist the data safely
	// when the process is exiting.
	//
	// see: https://github.com/edgexfoundry/app-functions-sdk-go/v2/issues/500
	go func() {
		signals := make(chan os.Signal)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		s := <-signals

		app.lgr.Info(fmt.Sprintf("Received '%s' signal from OS.", s.String()))
		cancel() // signal the taskLoop to finish
	}()

	// Subscribe to events.
	app.lgr.exitIfErr(app.service.SetFunctionsPipeline(app.processEdgeXEvent), "Failed to build pipeline.")
	app.lgr.exitIfErr(app.service.MakeItRun(), "Failed to run pipeline.")

	// let task loop complete
	wg.Wait()
	app.lgr.Info("Exiting.")


	// TODO: Replace below functions with built in and/or your custom functions for your use case.
	//       See https://docs.edgexfoundry.org/2.0/microservices/application/BuiltIn/ for list of built-in functions
	sample := functions.NewSample()
	err = app.service.SetFunctionsPipeline(
		transforms.NewFilterFor(deviceNames).FilterByDeviceName,
		sample.LogEventDetails,
		sample.ConvertEventToXML,
		sample.OutputXML)
	if err != nil {
		app.lgr.Errorf("SetFunctionsPipeline returned error: %s", err.Error())
		return -1
	}

	if err := app.service.MakeItRun(); err != nil {
		app.lgr.Errorf("MakeItRun returned error: %s", err.Error())
		return -1
	}

	// TODO: Do any required cleanup here, if needed

	return 0
}

func (app *inventoryApp) addRoutes() {
	for _, rte := range []struct {
		path, method string
		f            http.HandlerFunc // of course the EdgeX SDK doesn't take a http.Handler...
	}{
		{"/", http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, "static/html/index.html")
		}},
		{"/api/v1/readers", http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := app.defaultGrp.ListReaders(w); err != nil {
				app.lgr.Error("Failed to write readers list.", "error", err.Error())
				w.WriteHeader(http.StatusInternalServerError)
			}
		}},
		{"/api/v1/inventory/snapshot", http.MethodGet,
			func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := app.writeInventorySnapshot(w); err != nil {
					app.lgr.Error("Failed to write inventory snapshot.", "error", err.Error())
					w.WriteHeader(http.StatusInternalServerError)
				}
			},
		},
		{"/api/v1/command/reading/start", http.MethodPost,
			func(w http.ResponseWriter, req *http.Request) {
				if err := app.defaultGrp.StartAll(app.devService); err != nil {
					app.lgr.Error("Failed to StartAll.", "error", err.Error())
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			},
		},
		{"/api/v1/command/reading/stop", http.MethodPost,
			func(w http.ResponseWriter, req *http.Request) {
				if err := app.defaultGrp.StopAll(app.devService); err != nil {
					app.lgr.Error("Failed to StopAll.", "error", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			},
		},
		{"/api/v1/behaviors/{name}", http.MethodGet,
			func(w http.ResponseWriter, req *http.Request) {
				rv := mux.Vars(req)
				bName := rv["name"]
				// Currently, only "default" is supported.
				if bName != "default" {
					app.lgr.Error("Request to GET unknown behavior.", "name", bName)
					if _, err := w.Write([]byte("Invalid behavior name.")); err != nil {
						app.lgr.Error("Error writing failure response.", "error", err)
					}
					w.WriteHeader(http.StatusNotFound)
					return
				}

				data, err := json.Marshal(app.defaultGrp.Behavior())
				if err != nil {
					app.lgr.Error("Failed to marshal behavior.", "error", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				if _, err := w.Write(data); err != nil {
					app.lgr.Error("Failed to write behavior data.", "error", err)
					w.WriteHeader(http.StatusInternalServerError)
				}
			},
		},
		{"/api/v1/behaviors/{name}", http.MethodPut,
			func(w http.ResponseWriter, req *http.Request) {
				rv := mux.Vars(req)
				bName := rv["name"]
				// Currently, only "default" is supported.
				if bName != "default" {
					app.lgr.Error("Attempt to PUT unknown behavior.", "name", bName)
					if _, err := w.Write([]byte("Invalid behavior name.")); err != nil {
						app.lgr.Error("Error writing failure response.", "error", err)
					}
					w.WriteHeader(http.StatusNotFound)
					return
				}

				data, err := ioutil.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
				if err != nil {
					app.lgr.Error("Failed to read behavior body.", "error", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				var b llrp.Behavior
				if err := json.Unmarshal(data, &b); err != nil {
					app.lgr.Error("Failed to unmarshal behavior body.", "error", err,
						"body", string(data))
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(err.Error())) // best effort
					return
				}

				if err := app.defaultGrp.SetBehavior(app.devService, b); err != nil {
					app.lgr.Error("Failed to set new behavior.", "error", err)
					w.WriteHeader(http.StatusBadRequest)
					if _, err := w.Write([]byte(err.Error())); err != nil {
						app.lgr.Error("Error writing failure response.", "error", err)
					}
					return
				}

				app.lgr.Info("Updated behavior.", "name", bName)
			},
		},
	} {
		app.lgr.exitIfErr(app.service.AddRoute(rte.path, rte.f, rte.method),
			"Failed to add route.", lg{"path", rte.path}, lg{"method", rte.method})
	}
}

// getConfigClient returns a configuration client based on the command line args,
// or a default one if those lack a config provider URL.
// Ideally, a future version of the EdgeX SDKs will give us something like this
// without parsing the args again, but for now, this will do.
func getConfigClient() (configuration.Client, error) {
	sdkFlags := flags.New()
	sdkFlags.Parse(os.Args[1:])
	cpUrl, err := url.Parse(sdkFlags.ConfigProviderUrl())
	if err != nil {
		return nil, err
	}

	cpPort := 8500
	port := cpUrl.Port()
	if port != "" {
		cpPort, err = strconv.Atoi(port)
		if err != nil {
			return nil, errors.Wrap(err, "bad config port")
		}
	}

	configClient, err := configuration.NewConfigurationClient(types.ServiceConfig{
		Host:     cpUrl.Hostname(),
		Port:     cpPort,
		BasePath: BaseConsulPath,
		Type:     cpUrl.Scheme,
	})

	return configClient, errors.Wrap(err, "failed to get config client")
}

// processEdgeXEvent is used as the sole member of our pipeline.
// It's essentially our entrypoint for EdgeX event processing.
//
// Using the pipeline SDK is the least-effort method
// of accomplishing the grunt work of
// subscribing to EdgeX's event stream and
// accessing the resources that its agnosticism necessitates
// may come from any of several sources.
//
// But since it's a lot easier, safer, and more performant
// to write, call, compose, and test typical Go functions,
// we only use the SDK to call a single function (this one),
// which must verify the parameter types and arity,
// then verify the safety we lost by piping this through EdgeX by
// string matching the Event.Reading[].Name and JSON-unmarshal the Value string.
//
// Once we've reestablished these basic requirements,
// this dispatches the content to the appropriate type-safe functions.
func (app *inventoryApp) processEdgeXEvent(ctx interfaces.AppFunctionContext, data interface{}) (bool, interface{}) {
	event, ok := data.(models.Event)
	if !ok {
		// You know what's cool in compiled languages? Type safety.
		return false, errors.Errorf("expected an EdgeX Event, but got %T", event)
	}

	if len(event.Readings) < 1 {
		// Is this really an error? EdgeX's Filter functions say yes.
		return false, errors.New("event contains no Readings")
	}

	r := bytes.Buffer{}
	decoder := json.NewDecoder(&r)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()

	for i := range event.Readings {
		reading := &event.Readings[i] // Readings is 169 bytes. This avoid the copy.
		switch reading.Name {
		default:
			app.lgr.Debug("Unknown reading.", "reading", reading.Name)
			continue

		case ResourceReaderNotification:
			r.Reset()
			r.WriteString(reading.Value)
			notification := &llrp.ReaderEventNotification{}
			if err := decoder.Decode(notification); err != nil {
				app.lgr.Error("Failed to decode reader event notification", "error", err.Error())
				continue
			}

			if err := app.handleReaderEvent(event.Device, notification); err != nil {
				app.lgr.Error("Failed to handle ReaderEventNotification.",
					"error", err.Error(), "device", event.Device)
			}

		case ResourceROAccessReport:
			r.Reset()
			r.WriteString(reading.Value)

			report := &llrp.ROAccessReport{}
			if err := decoder.Decode(report); err != nil {
				app.lgr.Error("Failed to decode tag report",
					"error", err.Error(), "device", event.Device)
				continue
			}

			if report.TagReportData == nil {
				app.lgr.Warn("No tag report data in report.", "device", event.Device)
			} else {
				app.reports <- reportData{report, inventory.NewReportInfo(reading)}
				app.lgr.Trace("New ROAccessReport.",
					"device", event.Device, "tags", len(report.TagReportData))
			}
		}
	}

	return false, nil
}

// handleReaderEvent handles an llrp.ReaderEventNotification from the Device Service.
//
// If a device reports a new connection event,
// this adds the reader to the list of managed readers.
// If a device reports a close event, it removes that reader.
func (app *inventoryApp) handleReaderEvent(device string, notification *llrp.ReaderEventNotification) error {
	const connSuccess = llrp.ConnectionAttemptEvent(llrp.ConnSuccess)

	data := notification.ReaderEventNotificationData
	switch {
	case data.ConnectionAttemptEvent != nil && *data.ConnectionAttemptEvent == connSuccess:
		return app.defaultGrp.AddReader(app.devService, device)

	case data.ConnectionCloseEvent != nil:
		app.defaultGrp.RemoveReader(device)
	}

	return nil
}

// writeInventorySnapshot writes the current inventory snapshot to w.
func (app *inventoryApp) writeInventorySnapshot(w io.Writer) error {
	// We send w and a writeErr channel into the inventory execution context
	// and then wait to read a value from the writeErr channel.
	// That context closes writeErr to signal the snapshot is written to w
	// or an error prevented such, and we can send the result back to our caller.
	writeErr := make(chan error, 1)
	app.snapshotReqs <- snapshotDest{w, writeErr}
	return <-writeErr
}

// taskLoop is our main event loop for async processes
// that can't be modeled within the SDK's pipeline event loop.
//
// Namely, it launches scheduled tasks and configuration changes.
// Since nearly every round through this loop must read or write the inventory,
// this taskLoop ensures the modifications are done safely
// without requiring a ton of lock contention on the inventory itself.
func (app *inventoryApp) taskLoop(ctx context.Context, cfg config.ServiceConfig, lc logger.LoggingClient) {
	departedCheckSeconds := cfg.AppCustom.DepartedCheckIntervalSeconds
	aggregateDepartedTicker := time.NewTicker(time.Duration(departedCheckSeconds) * time.Second)
	ageoutTicker := time.NewTicker(1 * time.Hour)
	confErrCh := make(chan error)
	confUpdateCh := make(chan interface{})
	eventCh := make(chan []inventory.Event, eventChSz)

	defer func() {
		aggregateDepartedTicker.Stop()
		ageoutTicker.Stop()
		close(confErrCh)
		close(confUpdateCh)
	}()

	// load tag data
	var snapshot []inventory.StaticTag
	snapshotData, err := ioutil.ReadFile(filepath.Join(cacheFolder, tagCacheFile))
	if err != nil {
		lc.Warn("Failed to load inventory snapshot.", "error", err.Error())
	} else {
		if err := json.Unmarshal(snapshotData, &snapshot); err != nil {
			lc.Warn("Failed to unmarshal inventory snapshot.", "error", err.Error())
		}
	}

	processor := inventory.NewTagProcessor(lc, cfg, snapshot)
	if len(snapshot) > 0 {
		lc.Info(fmt.Sprintf("Restored %d tags from cache.", len(snapshot)))
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lc.Info("Starting event processor.")
		for events := range eventCh {
			if err := app.pushEventsToCoreData(ctx, events); err != nil {
				lc.Error("Failed to push events to CoreData.", "error", err.Error())
			}
		}
		lc.Info("Event processor stopped.")
	}()

	lc.Info("Starting task loop.")
	for {
		select {
		case <-ctx.Done():
			lc.Info("Stopping task loop.")
			close(eventCh)
			persistSnapshot(lc, snapshot)
			wg.Wait()
			lc.Info("Task loop stopped.")
			return

		case rd := <-app.reports:
			// TODO: we should refactor the ReaderGroup/TagReader
			//   to unite its tag processing with the TagProcessor code;
			//   the biggest goal is to perform only a single pass on the TagReportData.
			//   Secondarily, it would allow us to eliminate the ReaderGroup mutex.
			if !app.defaultGrp.ProcessTagReport(rd.info.DeviceName, rd.report.TagReportData) {
				// This can only happen if the device didn't exist when we started,
				// and we never got a Connection message for it.
				lc.Error("Tag Report for unknown device.", "device", rd.info.DeviceName)
			}

			events, updatedSnapshot := processor.ProcessReport(rd.report, rd.info)
			if updatedSnapshot != nil {
				snapshot = updatedSnapshot // always update the snapshot if available
			}
			if len(events) > 0 {
				persistSnapshot(lc, snapshot) // only persist when there are inventory events
				eventCh <- events
			}

		case t := <-aggregateDepartedTicker.C:
			if app.service.EventClient() == nil {
				lc.Info("Delaying AggregateDeparted processor: missing app-functions-sdk context.")
				break
			}
			lc.Debug("Running AggregateDeparted.", "time", fmt.Sprintf("%v", t))

			if events, updatedSnapshot := processor.AggregateDeparted(); len(events) > 0 {
				if updatedSnapshot != nil { // should always be true if there are events
					snapshot = updatedSnapshot
					persistSnapshot(lc, snapshot)
				}
				eventCh <- events
			}

		case t := <-ageoutTicker.C:
			lc.Debug("Running AgeOut.", "time", fmt.Sprintf("%v", t))
			if _, updatedSnapshot := processor.AgeOut(); updatedSnapshot != nil {
				snapshot = updatedSnapshot
				persistSnapshot(lc, snapshot)
			}
		case req := <-app.snapshotReqs:
			data, err := json.Marshal(snapshot)
			if err == nil {
				_, err = req.w.Write(data) // only write if there was no error already
			}
			req.result <- err

		case err := <-confErrCh:
			lc.Error("Configuration error.", "error", err.Error())
		}
	}
}

func persistSnapshot(lc logger.LoggingClient, snapshot []inventory.StaticTag) {
	lc.Debug("Persisting inventory snapshot.")
	data, err := json.Marshal(snapshot)
	if err != nil {
		lc.Warn("Failed to marshal inventory snapshot.", "error", err.Error())
		return
	}

	if err := ioutil.WriteFile(filepath.Join(cacheFolder, tagCacheFile), data, filePerm); err != nil {
		lc.Warn("Failed to persist inventory snapshot.", "error", err.Error())
		return
	}
	lc.Info("Persisted inventory snapshot.", "tags", len(snapshot))
}

// setDefaultBehavior sets the behavior associated with the default device group.
func (app *inventoryApp) setDefaultBehavior(b llrp.Behavior) error {
	app.devMu.Lock()
	err := app.defaultGrp.SetBehavior(app.devService, b)
	app.devMu.Unlock()
	return err
}

// pushEventsToCoreData will send one or more Inventory Events as a single EdgeX Event with
// an EdgeX Reading for each Inventory Event
func (app *inventoryApp) pushEventsToCoreData(ctx context.Context, events []inventory.Event) error {
	eventClient := app.service.EventClient()
	if eventClient == nil {
		return errors.New("unable to push events to core data: app.service.EventClient returned nil")
	}

	now := time.Now().UnixNano()
	readings := make([]models.Reading, 0, len(events))

	var errs []error
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			errs = append(errs, errors.Wrap(err, "error marshalling event"))
			continue
		}

		resourceName := ResourceInventoryEvent + string(event.OfType())
		app.lgr.Info()
		app.service.LoggingClient.Info("Sending Inventory Event.",
			"type", resourceName, "payload", string(payload))

		readings = append(readings, models.Reading{
			Value:  string(payload),
			Origin: now,
			Device: eventDeviceName,
			Name:   resourceName,
		})
	}

	edgeXEvent := &models.Event{
		Device:   eventDeviceName,
		Origin:   now,
		Readings: readings,
	}

	ctx, cancel := context.WithTimeout(ctx, coreDataPostTimeout)
	defer cancel()

	if _, err := eventClient.Add(ctx, edgeXEvent); err != nil {
		errs = append(errs, errors.Wrap(err, "unable to push inventory event(s) to core-data"))
	}

	if errs != nil {
		return llrp.MultiErr(errs)
	}
	return nil
}
