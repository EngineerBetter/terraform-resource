package in

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"

	"github.com/ljfranklin/terraform-resource/in/models"
	baseModels "github.com/ljfranklin/terraform-resource/models"
	"github.com/ljfranklin/terraform-resource/storage"
	"github.com/ljfranklin/terraform-resource/terraform"
)

type Runner struct {
	OutputDir string
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

	storageModel := req.Source.Storage
	if err = storageModel.Validate(); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to validate storage Model: %s", err)
	}
	storageDriver := storage.BuildDriver(storageModel)

	stateFilename := fmt.Sprintf("%s.tfstate", req.Version.EnvName)
	terraformModel := terraform.Model{
		StateFileLocalPath:  path.Join(tmpDir, "terraform.tfstate"),
		StateFileRemotePath: stateFilename,
	}
	if err = terraformModel.Validate(); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to validate terraform Model: %s", err)
	}

	client := terraform.Client{
		Model:         terraformModel,
		StorageDriver: storageDriver,
	}

	storageVersion, err := client.DownloadStateFileIfExists()
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to download state file from storage backend: %s", err)
	}
	if storageVersion.IsZero() {
		return models.InResponse{}, fmt.Errorf("StateFile does not exist with key '%s'", stateFilename)
	}
	version := baseModels.NewVersion(storageVersion)

	output, err := client.Output()
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to parse terraform output.\nError: %s", err)
	}

	outputFilepath := path.Join(r.OutputDir, "metadata")
	outputFile, err := os.Create(outputFilepath)
	if err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to create output file at path '%s': %s", outputFilepath, err)
	}
	if err := json.NewEncoder(outputFile).Encode(output); err != nil {
		return models.InResponse{}, fmt.Errorf("Failed to write output file: %s", err)
	}

	metadata := []models.MetadataField{}
	for key, value := range output {
		metadata = append(metadata, models.MetadataField{
			Name:  key,
			Value: value,
		})
	}

	resp := models.InResponse{
		Version:  version,
		Metadata: metadata,
	}
	return resp, nil
}