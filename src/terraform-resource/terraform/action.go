package terraform

import (
	"encoding/json"
	"fmt"
	"strings"
	"terraform-resource/logger"
	"terraform-resource/models"
)

type Action struct {
	Client          Client
	Logger          logger.Logger
	EnvName         string
	DeleteOnFailure bool
}

type Result struct {
	Version models.Version
	Output  map[string]map[string]interface{}
}

func (r Result) RawOutput() map[string]interface{} {
	outputs := map[string]interface{}{}
	for key, value := range r.Output {
		outputs[key] = value["value"]
	}

	return outputs
}

func (r Result) SanitizedOutput() map[string]string {
	output := map[string]string{}
	for key, value := range r.Output {
		if value["sensitive"] == true {
			output[key] = "<sensitive>"
		} else {
			jsonValue, err := json.Marshal(value["value"])
			if err != nil {
				jsonValue = []byte(fmt.Sprintf("Unable to parse output value for key '%s': %s", key, err))
			}

			output[key] = strings.Trim(string(jsonValue), "\"")
		}
	}
	return output
}

func (a *Action) Apply() (Result, error) {
	err := a.setup()
	if err != nil {
		return Result{}, err
	}

	result, err := a.attemptApply()
	if err != nil {
		a.Logger.Error("Failed To Run Terraform Apply!")
		err = fmt.Errorf("Apply Error: %s", err)
	}

	if err != nil && a.DeleteOnFailure {
		a.Logger.Warn("Cleaning Up Partially Created Resources...")

		_, destroyErr := a.attemptDestroy()
		if destroyErr != nil {
			a.Logger.Error("Failed To Run Terraform Destroy!")
			err = fmt.Errorf("%s\nDestroy Error: %s", err, destroyErr)
		}
	}

	if err == nil {
		a.Logger.Success("Successfully Ran Terraform Apply!")
	}

	return result, err
}

func (a *Action) attemptApply() (Result, error) {
	a.Logger.InfoSection("Terraform Apply")
	defer a.Logger.EndSection()

	if err := a.createWorkspaceIfNotExists(); err != nil {
		return Result{}, err
	}

	if err := a.Client.Apply(); err != nil {
		return Result{}, err
	}

	serial, err := a.currentSerial()
	if err != nil {
		return Result{}, err
	}
	clientOutput, err := a.Client.Output(a.EnvName)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Output: clientOutput,
		Version: models.Version{
			EnvName: a.EnvName,
			Serial:  serial,
		},
	}, nil
}

func (a *Action) Destroy() (Result, error) {
	err := a.setup()
	if err != nil {
		return Result{}, err
	}

	result, err := a.attemptDestroy()
	if err == nil {
		a.Logger.Success("Successfully Ran Terraform Destroy!")
	}

	return result, err
}

func (a *Action) attemptDestroy() (Result, error) {
	a.Logger.WarnSection("Terraform Destroy")
	defer a.Logger.EndSection()

	if err := a.Client.Destroy(); err != nil {
		return Result{}, err
	}

	if err := a.Client.WorkspaceDelete(a.EnvName); err != nil {
		return Result{}, err
	}

	return Result{
		Output: map[string]map[string]interface{}{},
		Version: models.Version{
			EnvName: a.EnvName,
		},
	}, nil
}

func (a *Action) setup() error {
	if err := a.Client.InitWithBackend(); err != nil {
		return err
	}

	if err := a.Client.Import(a.EnvName); err != nil {
		return err
	}

	return nil
}

func (a *Action) currentSerial() (int, error) {
	rawState, err := a.Client.StatePull(a.EnvName)
	if err != nil {
		return -1, err
	}

	// TODO: read this into a struct
	tfState := map[string]interface{}{}
	if err = json.Unmarshal(rawState, &tfState); err != nil {
		return -1, fmt.Errorf("Failed to unmarshal JSON output.\nError: %s\nOutput: %s", err, rawState)
	}

	serial, ok := tfState["serial"].(float64)
	if !ok {
		return -1, fmt.Errorf("Expected number value for 'serial' but got '%#v'", tfState["serial"])
	}

	return int(serial), nil
}

func (a *Action) createWorkspaceIfNotExists() error {
	workspaces, err := a.Client.WorkspaceList()
	if err != nil {
		return err
	}

	workspaceExists := false
	for _, space := range workspaces {
		if space == a.EnvName {
			workspaceExists = true
		}
	}

	if !workspaceExists {
		return a.Client.WorkspaceNew(a.EnvName)
	}

	return nil
}
