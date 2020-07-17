package routes

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gorilla/mux"
)

var client = NewHTTPClient()

// PingResponse sends pong back to client indicating service is up
func PingResponse(writer http.ResponseWriter, req *http.Request) {

	responseMessage := "pong"
	httpResponseCode := http.StatusOK

	logger, _, err := GetSettingsHandler(req)

	if err != nil {
		httpResponseCode = http.StatusInternalServerError
		responseMessage = http.StatusText(httpResponseCode)
		if logger != nil {
			logger.Error(err.Error())
		} else {
			fmt.Fprintln(os.Stderr, err.Error())
		}
	}
	if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
		logger.Error(werr.Error())
	}

}

// GetSensorsCommand gets sensor list via EdgeX Core Command API
func GetSensorsCommand(writer http.ResponseWriter, req *http.Request) {

	//Initialize response parameters
	responseMessage := ""
	httpResponseCode := http.StatusOK

	logger, appSettings, err := GetSettingsHandler(req)
	if err != nil {
		httpResponseCode = http.StatusInternalServerError
		responseMessage = http.StatusText(httpResponseCode)
		if logger != nil {
			logger.Error(err.Error())
		} else {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
			logger.Error(werr.Error())
		}
		return
	}

	logger.Info("Command to get all the sensors called")
	deviceList, err := SendHTTPGetDeviceRequest(appSettings, client)

	if err != nil {
		//Log the actual error & display response message to Client
		httpResponseCode = http.StatusInternalServerError
		if deviceList != nil {
			responseMessage = err.Error()
		} else {
			responseMessage = http.StatusText(http.StatusInternalServerError)
		}
		logger.Error(err.Error())
		if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
			logger.Error(werr.Error())
		}
	} else {
		//Send list of registered rfid devices to Client request
		if werr := WriteJSONDeviceListHTTPResponse(writer, req, deviceList); werr != nil {
			logger.Error(werr.Error())
		}
	}

}

// IssueReadCommand sends start/stop reading command via EdgeX Core Command API
func IssueReadCommand(writer http.ResponseWriter, req *http.Request) {

	//Initialize response parameters
	responseMessage := ""
	httpResponseCode := http.StatusOK

	logger, appSettings, err := GetSettingsHandler(req)

	if err != nil {
		responseMessage = http.StatusText(http.StatusInternalServerError)
		httpResponseCode = http.StatusInternalServerError
		if logger != nil {
			logger.Error(err.Error())
		} else {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
			logger.Error(werr.Error())
		}
		return
	}

	putCommandEndpoint := appSettings[CoreCommandPUTDevicesNameCommandEndpoint]
	//Check for empty putCommandEndpoint
	if strings.TrimSpace(putCommandEndpoint) == "" {
		responseMessage = http.StatusText(http.StatusInternalServerError)
		httpResponseCode = http.StatusInternalServerError
		logger.Error("PUT command Endpoint to EdgeX Core is nil")
		if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
			logger.Error(werr.Error())
		}
		return
	}

	vars := mux.Vars(req)
	readCommand := vars[ReadCommand]

	logger.Info(fmt.Sprintf("readCommand to be sent to registered devices is %s", readCommand))

	//Return back with error message if unable to parse Read Command
	if !(readCommand == StartReadingCommand || readCommand == StopReadingCommand) {

		responseMessage = fmt.Sprintf("Unable to parse %v Command", readCommand)
		httpResponseCode = http.StatusBadRequest
		logger.Error(responseMessage)

		//Send response back to Client requent
		if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
			logger.Error(werr.Error())
		}
		return

	}

	//Get Device List from EdgeX Core Command
	deviceList, err := SendHTTPGetDeviceRequest(appSettings, client)

	if err != nil {
		//Log the actual error & display response message to Client as "Internal Server Error"
		if deviceList != nil {
			responseMessage = err.Error()
		} else {
			responseMessage = http.StatusText(http.StatusInternalServerError)
		}
		httpResponseCode = http.StatusInternalServerError
		logger.Error(err.Error())

		if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
			logger.Error(werr.Error())
		}
		return
	}

	//Empty device List check done in SendHTTPGetRequest function, error logged in 122 & return back
	deviceListLength := len(deviceList)

	//sendErrs track any unsuccessful PUT request to EdgeX Core Command
	sendErrs := make([]bool, deviceListLength)

	//Create & Add devices count into waitgroup
	var wg sync.WaitGroup
	wg.Add(deviceListLength)

	logger.Info(fmt.Sprintf("Sending %v Command (PUT request) to all rfid registered devices", readCommand))

	for i, deviceName := range deviceList {
		go func(i int, deviceName string) {

			//Delete from waitgroup
			defer wg.Done()

			//PUT request to device-deviceName via EdgeX Core Command
			err := SendHTTPPUTRequest(readCommand, putCommandEndpoint, deviceName, logger, client)
			if err != nil {
				sendErrs[i] = true
				logger.Error(fmt.Sprintf("Error sending %v Command to device %v via EdgeX Core-Command", readCommand, deviceName))
			}
		}(i, deviceName)
	}

	//Wait until all in waitgroup are executed
	wg.Wait()

	//Successful Response back to Client Request
	responseMessage = fmt.Sprintf("Successfully sent %v Command to all registered rfid devices via EdgeX Core-Command", readCommand)
	for _, errYes := range sendErrs {
		if errYes {
			//Unsuccessful Response back to Client Request
			httpResponseCode = http.StatusInternalServerError
			responseMessage = fmt.Sprintf("Unsuccessful in sending %v Command", readCommand)
			break
		}

	}

	//Send response back to Client requent
	if werr := WritePlainTextHTTPResponse(writer, req, responseMessage, httpResponseCode); werr != nil {
		logger.Error(werr.Error())
	}
}

// IssueBehaviorCommand sends command to set/apply behavior command
func IssueBehaviorCommand(writer http.ResponseWriter, req *http.Request) {
	//TODO
}
