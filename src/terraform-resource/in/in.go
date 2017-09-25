package in

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"

	"terraform-resource/encoder"
	"terraform-resource/models"
	"terraform-resource/storage"
	"terraform-resource/terraform"
)

type Runner struct {
	OutputDir string
	LogWriter io.Writer
}

func (r Runner) Run(req models.InRequest) (models.InResponse, error) {
	if err := req.Version.Validate(); err != nil {
		return models.InResponse{}, fmt.Errorf("Invalid Version request: %s", err)
	}

	if req.Params.Action == models.DestroyAction {
		resp := models.InResponse{
			Version: req.Version,
		}
		return resp, nil
	}

	tmpDir, err := ioutil.TempDir(os.TempDir(), "terraform-resource-in")
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to create tmp dir at '%s'", os.TempDir())
	}
	defer os.RemoveAll(tmpDir)

	var resp models.InResponse
	if req.Source.BackendType != "" {
		resp, err = r.inWithBackend(req, tmpDir)
	} else {
		resp, err = r.inWithLegacyStorage(req, tmpDir)
	}
	if err != nil {
		return models.InResponse{}, err
	}

	nameFilepath := path.Join(r.OutputDir, "name")
	nameFile, err := os.Create(nameFilepath)
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to create name file at path '%s': %s", nameFilepath, err)
	}
	defer nameFile.Close()
	nameFile.WriteString(req.Version.EnvName)

	return resp, nil
}

func (r Runner) inWithBackend(req models.InRequest, tmpDir string) (models.InResponse, error) {
	if req.Version.IsPlan() {
		// TODO: should we output plan file here?
		resp := models.InResponse{
			Version: req.Version,
		}
		return resp, nil
	}

	terraformModel := req.Source.Terraform
	if err := terraformModel.Validate(); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to validate terraform Model: %s", err)
	}
	terraformModel.Source = "."
	if req.Params.OutputModule != "" {
		terraformModel.OutputModule = req.Params.OutputModule
	}

	client := terraform.NewClient(
		terraformModel,
		r.LogWriter,
	)

	targetEnvName := req.Version.EnvName
	if err := client.InitWithBackend(targetEnvName); err != nil {
		return models.InResponse{}, err
	}

	spaces, err := client.WorkspaceList()
	if err != nil {
		return models.InResponse{}, err
	}
	foundEnv := false
	for _, space := range spaces {
		if space == targetEnvName {
			foundEnv = true
		}
	}
	if !foundEnv {
		return models.InResponse{}, fmt.Errorf(
			"Workspace '%s' does not exist in backend."+
				"\nIf you intended to run the `destroy` action, add `put.get_params.action: destroy`."+
				"\nThis is a temporary requirement until Concourse supports a `delete` step.",
			targetEnvName,
		)
	}

	tfOutput, err := client.Output(targetEnvName)
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to parse terraform output.\nError: %s", err)
	}
	result := terraform.Result{
		Output: tfOutput,
	}

	outputFilepath := path.Join(r.OutputDir, "metadata")
	outputFile, err := os.Create(outputFilepath)
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to create output file at path '%s': %s", outputFilepath, err)
	}

	if err = encoder.NewJSONEncoder(outputFile).Encode(result.RawOutput()); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to write output file: %s", err)
	}

	metadata := []models.MetadataField{}
	for key, value := range result.SanitizedOutput() {
		metadata = append(metadata, models.MetadataField{
			Name:  key,
			Value: value,
		})
	}

	tfVersion, err := client.Version()
	if err != nil {
		return models.InResponse{}, err
	}
	metadata = append(metadata, models.MetadataField{
		Name:  "terraform_version",
		Value: tfVersion,
	})

	if req.Params.OutputStatefile {
		stateFilePath := path.Join(r.OutputDir, "terraform.tfstate")
		stateContents, err := client.StatePull(targetEnvName)
		if err != nil {
			return models.InResponse{}, err
		}
		err = ioutil.WriteFile(stateFilePath, stateContents, 0777)
		if err != nil {
			return models.InResponse{}, err
		}
	}

	resp := models.InResponse{
		// TODO: is this the right Version?
		Version:  req.Version,
		Metadata: metadata,
	}
	return resp, nil
}

func (r Runner) inWithLegacyStorage(req models.InRequest, tmpDir string) (models.InResponse, error) {
	storageModel := req.Source.Storage
	if err := storageModel.Validate(); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to validate storage Model: %s", err)
	}
	storageDriver := storage.BuildDriver(storageModel)

	stateFilename := fmt.Sprintf("%s.tfstate", req.Version.EnvName)
	storageVersion, err := storageDriver.Version(stateFilename)
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to check for existing state file: %s", err)
	}
	if storageVersion.IsZero() {
		if req.Version.IsPlan() {
			resp := models.InResponse{
				Version: req.Version,
			}
			return resp, nil
		}

		return models.InResponse{}, fmt.Errorf(
			"State file does not exist with key '%s'."+
				"\nIf you intended to run the `destroy` action, add `put.get_params.action: destroy`."+
				"\nThis is a temporary requirement until Concourse supports a `delete` step.",
			stateFilename,
		)
	}

	terraformModel := models.Terraform{
		StateFileLocalPath:  path.Join(tmpDir, "terraform.tfstate"),
		StateFileRemotePath: stateFilename,
	}

	if req.Params.OutputModule != "" {
		terraformModel.OutputModule = req.Params.OutputModule
	}

	if err = terraformModel.Validate(); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to validate terraform Model: %s", err)
	}

	client := terraform.NewClient(
		terraformModel,
		r.LogWriter,
	)
	stateFile := storage.StateFile{
		LocalPath:     terraformModel.StateFileLocalPath,
		RemotePath:    terraformModel.StateFileRemotePath,
		StorageDriver: storageDriver,
	}

	storageVersion, err = stateFile.Download()
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to download state file from storage backend: %s", err)
	}
	version := models.NewVersionFromLegacyStorage(storageVersion)

	tfOutput, err := client.OutputWithLegacyStorage()
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to parse terraform output.\nError: %s", err)
	}
	result := terraform.Result{
		Output: tfOutput,
	}

	outputFilepath := path.Join(r.OutputDir, "metadata")
	outputFile, err := os.Create(outputFilepath)
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to create output file at path '%s': %s", outputFilepath, err)
	}

	if err = encoder.NewJSONEncoder(outputFile).Encode(result.RawOutput()); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to write output file: %s", err)
	}

	metadata := []models.MetadataField{}
	for key, value := range result.SanitizedOutput() {
		metadata = append(metadata, models.MetadataField{
			Name:  key,
			Value: value,
		})
	}

	tfVersion, err := client.Version()
	if err != nil {
		return models.InResponse{}, err
	}
	metadata = append(metadata, models.MetadataField{
		Name:  "terraform_version",
		Value: tfVersion,
	})

	if req.Params.OutputStatefile {
		stateFilePath := path.Join(r.OutputDir, "terraform.tfstate")
		stateContents, err := ioutil.ReadFile(terraformModel.StateFileLocalPath)
		if err != nil {
			return models.InResponse{}, err
		}
		err = ioutil.WriteFile(stateFilePath, stateContents, 0777)
		if err != nil {
			return models.InResponse{}, err
		}
	}

	resp := models.InResponse{
		Version:  version,
		Metadata: metadata,
	}
	return resp, nil

}
