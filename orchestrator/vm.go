package function

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

const startupScriptTemplate = `#!/bin/bash
set -euo pipefail

METADATA_URL="http://metadata.google.internal/computeMetadata/v1"
METADATA_HEADER="Metadata-Flavor: Google"

JIT_CONFIG=$(curl -sf -H "${METADATA_HEADER}" "${METADATA_URL}/instance/attributes/jit-config")
curl -sf -X DELETE -H "${METADATA_HEADER}" \
  "${METADATA_URL}/instance/attributes/jit-config" || true

CACHE_BUCKET="%s"
REPO_OWNER="%s"
REPO_NAME="%s"

cd /home/runner

if [ -n "${CACHE_BUCKET}" ] && [ -x /usr/local/bin/cache-server ]; then
  /usr/local/bin/cache-server \
    -bucket "${CACHE_BUCKET}" \
    -owner "${REPO_OWNER}" \
    -repo "${REPO_NAME}" &
  for i in $(seq 1 10); do
    curl -sf http://localhost:8787/health && break
    sleep 0.5
  done
  export ACTIONS_RESULTS_URL="http://localhost:8787/"
  export ACTIONS_CACHE_SERVICE_V2=true
fi

export HOME=/home/runner
if [ -f /etc/environment ]; then
  set -a
  . /etc/environment
  set +a
fi

sudo -u runner -E ./run.sh --jitconfig "${JIT_CONFIG}"
`

var createInstanceForRunner = createInstance

func createRunnerVM(ctx context.Context, event WorkflowJobEvent, labels *RunnerLabels, credentials installationCredentials) error {
	if err := validateRunnerVMPreflight(labels); err != nil {
		return err
	}

	owner := event.Repository.Owner.Login
	repo := event.Repository.Name
	repoFullName := event.Repository.FullName

	instanceName := fmt.Sprintf("gcrunner-%d-%d", event.WorkflowJob.RunID, event.WorkflowJob.ID)

	jitConfig, err := githubAPIClient.generateJITConfig(ctx, owner, repo, instanceName, event.WorkflowJob.Labels, credentials)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}

	cacheBucket := os.Getenv("GCRUNNER_CACHE_BUCKET")
	startupScript := fmt.Sprintf(startupScriptTemplate, cacheBucket, owner, repo)

	region := os.Getenv("GCE_REGION")
	if region == "" {
		region = "us-central1"
	}

	project := os.Getenv("GCP_PROJECT")

	zones, zoneErr := runnerVMZones(ctx, project, region, labels, ListZones)
	if zoneErr != nil {
		log.Printf("WARN zone_discovery_failed region=%s error=%v", region, zoneErr)
	}

	return createVMInZones(zones, func(zone string) error {
		machineType := labels.Machine
		if labels.MachineMode != "exact" {
			resolved, err := ResolveMachineType(ctx, project, zone, labels)
			if err != nil {
				return fmt.Errorf("resolve machine type in %s: %w", zone, err)
			}
			machineType = resolved
		}

		err := createInstanceForRunner(ctx, instanceName, zone, machineType, labels, startupScript, jitConfig)
		if err == nil {
			log.Printf("Created VM %s in %s (type=%s) for %s", instanceName, zone, machineType, repoFullName)
		}
		return err
	})
}

func createVMInZones(zones []string, create func(string) error) error {
	var lastErr error
	allMachineResourcesUnavailable := len(zones) > 0
	for _, zone := range zones {
		err := create(zone)
		if err == nil {
			return nil
		}

		var creationErr *vmCreationError
		kind := insertErrorRetryable
		if errors.As(err, &creationErr) {
			kind = creationErr.kind
		} else {
			kind = classifyInsertError(err)
		}
		switch kind {
		case insertErrorAlreadyExists:
			log.Printf("VM already exists in %s (duplicate webhook), skipping", zone)
			return nil
		case insertErrorNoMachineType:
			lastErr = err
			log.Printf("No matching machine type in %s: %v, trying next zone", zone, err)
			continue
		case insertErrorFatal:
			if isResourceNotFoundError(err) {
				lastErr = err
				log.Printf("Machine resource not found in %s: %v, trying next zone", zone, err)
				continue
			}
			return &vmCreationError{
				kind: kind,
				err:  fmt.Errorf("failed to create VM in %s: %w", zone, err),
			}
		case insertErrorQuota:
			return &vmCreationError{
				kind: kind,
				err:  fmt.Errorf("failed to create VM in %s: %w", zone, err),
			}
		default:
			allMachineResourcesUnavailable = false
			lastErr = err
			log.Printf("Failed to create VM in %s: %v, trying next zone", zone, err)
		}
	}

	if allMachineResourcesUnavailable {
		return &vmCreationError{
			kind: insertErrorFatal,
			err:  fmt.Errorf("no matching machine type or machine resource in any zone: %w", lastErr),
		}
	}
	return fmt.Errorf("failed to create VM in any zone: %w", lastErr)
}

func createInstance(ctx context.Context, name, zone, machineType string, labels *RunnerLabels, startupScript, jitConfig string) error {
	if labels.KVM {
		if err := validateKVMSupported(machineType); err != nil {
			return &vmCreationError{
				kind: insertErrorFatal,
				err:  fmt.Errorf("validate nested virtualization support: %w", err),
			}
		}
	}

	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("WARN compute_client_close_failed operation=create_instance error=%v", err)
		}
	}()

	project := os.Getenv("GCP_PROJECT")
	machineType = fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType)
	sourceImage := resolveSourceImage(labels.Image)

	diskSizeGB, err := parseDiskSize(labels.Disk)
	if err != nil {
		return &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("parse disk label: %w", err)}
	}

	instance := &computepb.Instance{
		Name:        proto.String(name),
		MachineType: proto.String(machineType),
		Disks: []*computepb.AttachedDisk{
			{
				AutoDelete: proto.Bool(true),
				Boot:       proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					SourceImage: proto.String(sourceImage),
					DiskSizeGb:  proto.Int64(diskSizeGB),
					DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/%s", zone, labels.DiskType)),
				},
			},
		},
		NetworkInterfaces: []*computepb.NetworkInterface{
			{
				AccessConfigs: []*computepb.AccessConfig{
					{
						Name: proto.String("External NAT"),
						Type: proto.String("ONE_TO_ONE_NAT"),
					},
				},
			},
		},
		Metadata: &computepb.Metadata{
			Items: []*computepb.Items{
				{
					Key:   proto.String("startup-script"),
					Value: proto.String(startupScript),
				},
				{
					Key:   proto.String("jit-config"),
					Value: proto.String(jitConfig),
				},
			},
		},
		Labels: map[string]string{
			"gcrunner": "true",
		},
		ServiceAccounts: []*computepb.ServiceAccount{
			{
				Email: proto.String(fmt.Sprintf("gcrunner-runner@%s.iam.gserviceaccount.com", project)),
				Scopes: []string{
					"https://www.googleapis.com/auth/cloud-platform",
				},
			},
		},
	}

	if labels.Spot {
		instance.Scheduling = &computepb.Scheduling{
			ProvisioningModel:         proto.String("SPOT"),
			InstanceTerminationAction: proto.String("DELETE"),
		}
	}

	if labels.KVM {
		instance.AdvancedMachineFeatures = &computepb.AdvancedMachineFeatures{
			EnableNestedVirtualization: proto.Bool(true),
		}
	}

	op, err := client.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          project,
		Zone:             zone,
		InstanceResource: instance,
	})
	if err != nil {
		return err
	}

	return op.Wait(ctx)
}

func runnerVMZones(ctx context.Context, project, region string, labels *RunnerLabels, listZones func(context.Context, string, string) ([]string, error)) ([]string, error) {
	if labels.Zone != "" {
		return strings.Split(labels.Zone, "+"), nil
	}

	zones, err := listZones(ctx, project, region)
	if err != nil {
		return []string{region + "-a", region + "-b", region + "-c"}, err
	}
	return zones, nil
}

func deleteRunnerVM(ctx context.Context, name string, labels *RunnerLabels) error {
	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("WARN compute_client_close_failed operation=delete_instance error=%v", err)
		}
	}()

	project := os.Getenv("GCP_PROJECT")
	region := os.Getenv("GCE_REGION")
	if region == "" {
		region = "us-central1"
	}
	zones, zoneErr := runnerVMZones(ctx, project, region, labels, ListZones)
	if zoneErr != nil {
		log.Printf("WARN zone_discovery_failed region=%s error=%v", region, zoneErr)
	}

	return deleteVMInZones(name, zones, func(zone string) error {
		op, err := client.Delete(ctx, &computepb.DeleteInstanceRequest{
			Project:  project,
			Zone:     zone,
			Instance: name,
		})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

func deleteVMInZones(name string, zones []string, delete func(string) error) error {
	var lastErr error
	for _, zone := range zones {
		err := delete(zone)
		if err == nil {
			log.Printf("Deleted VM %s in %s", name, zone)
			return nil
		}
		if isGoogleAPINotFoundError(err) {
			continue
		}
		lastErr = err
		log.Printf("WARN: failed to delete VM %s in %s: %v, trying next zone", name, zone, err)
	}

	if lastErr != nil {
		return fmt.Errorf("delete VM %s in any zone: %w", name, lastErr)
	}
	log.Printf("VM %s not found in any zone, may have already been deleted", name)
	return nil
}

func resolveSourceImage(image string) string {
	imageProject := os.Getenv("GCRUNNER_IMAGE_PROJECT")
	if imageProject == "" {
		imageProject = "gcrunner-images"
	}
	imageMap := map[string]string{
		"ubuntu24-full-x64": "gcrunner-ubuntu2404-x64",
		"ubuntu22-full-x64": "gcrunner-ubuntu2204-x64",
	}
	if family, ok := imageMap[image]; ok {
		return fmt.Sprintf("projects/%s/global/images/family/%s", imageProject, family)
	}
	if strings.Contains(image, "/") {
		return image
	}
	return "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
}

func parseDiskSize(disk string) (int64, error) {
	disk = strings.TrimSuffix(strings.ToLower(disk), "gb")
	size, err := strconv.ParseInt(disk, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid disk size %q: %w", disk, err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("disk size must be positive: %d", size)
	}
	if size < 10 {
		size = 50
	}
	return size, nil
}

func validateRunnerLabels(labels *RunnerLabels) error {
	if labels.MachineMode != "exact" {
		if _, _, err := parseRange(labels.CPU); err != nil {
			return &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("parse cpu label: %w", err)}
		}
		if _, _, err := parseRange(labels.RAM); err != nil {
			return &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("parse ram label: %w", err)}
		}
	}
	if _, err := parseDiskSize(labels.Disk); err != nil {
		return &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("parse disk label: %w", err)}
	}
	if labels.Family != "" {
		for _, family := range strings.Split(labels.Family, "+") {
			if family == "" {
				return &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("empty machine family component in %q", labels.Family)}
			}
		}
	}
	if labels.Zone != "" {
		for _, zone := range strings.Split(labels.Zone, "+") {
			if zone == "" {
				return &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("empty zone component in %q", labels.Zone)}
			}
		}
	}
	return nil
}

func validateRunnerVMPreflight(labels *RunnerLabels) error {
	if err := validateRunnerLabels(labels); err != nil {
		return err
	}
	if !labels.KVM {
		return nil
	}
	if labels.MachineMode == "exact" {
		if err := validateKVMSupported(labels.Machine); err != nil {
			return &vmCreationError{
				kind: insertErrorFatal,
				err:  fmt.Errorf("validate nested virtualization support: %w", err),
			}
		}
		return nil
	}
	if labels.MachineMode == "family" && onlyKVMUnsupportedFamilies(labels.Family) {
		return &vmCreationError{
			kind: insertErrorFatal,
			err:  fmt.Errorf("nested virtualization (kvm=true) not supported on requested machine families %q", labels.Family),
		}
	}
	return nil
}

func onlyKVMUnsupportedFamilies(family string) bool {
	for _, candidate := range strings.Split(family, "+") {
		if !kvmUnsupportedFamilies[candidate] {
			return false
		}
	}
	return true
}

type vmCreationError struct {
	kind insertErrorKind
	err  error
}

func (e *vmCreationError) Error() string {
	return e.err.Error()
}

func (e *vmCreationError) Unwrap() error {
	return e.err
}

func isFatalVMCreationError(err error) bool {
	var creationErr *vmCreationError
	return errors.As(err, &creationErr) && creationErr.kind == insertErrorFatal
}

type insertErrorKind int

const (
	insertErrorRetryable insertErrorKind = iota
	insertErrorQuota
	insertErrorFatal
	insertErrorAlreadyExists
	insertErrorNoMachineType
)

var kvmUnsupportedFamilies = map[string]bool{
	"e2":  true,
	"t2d": true,
	"t2a": true,
}

func validateKVMSupported(machineType string) error {
	family, _ := parseMachineFamily(machineType)
	if kvmUnsupportedFamilies[family] {
		return fmt.Errorf("nested virtualization (kvm=true) not supported on machine family %q (type %q); use n1/n2/n2d/c2/c3/c3d", family, machineType)
	}
	return nil
}

func isResourceNotFoundError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return isGoogleAPINotFoundError(err)
	}
	return strings.Contains(err.Error(), "RESOURCE_NOT_FOUND")
}

func isGoogleAPINotFoundError(err error) bool {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Code == 404 {
		return true
	}
	for _, detail := range apiErr.Errors {
		if detail.Reason == "notFound" {
			return true
		}
	}
	return false
}

func classifyInsertError(err error) insertErrorKind {
	if err == nil {
		return insertErrorRetryable
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		for _, detail := range apiErr.Errors {
			switch detail.Reason {
			case "quotaExceeded":
				return insertErrorQuota
			case "notFound", "invalid", "invalidArgument", "forbidden", "permissionDenied", "insufficientPermissions":
				return insertErrorFatal
			case "alreadyExists":
				return insertErrorAlreadyExists
			}
		}
		if apiErr.Code == 400 || apiErr.Code == 401 || apiErr.Code == 404 {
			return insertErrorFatal
		}
		if apiErr.Code == 409 {
			return insertErrorAlreadyExists
		}
		return insertErrorRetryable
	}

	msg := err.Error()
	lowerMsg := strings.ToLower(msg)
	if strings.Contains(msg, "QUOTA_EXCEEDED") {
		return insertErrorQuota
	}
	if strings.Contains(msg, "alreadyExists") || strings.Contains(msg, "ALREADY_EXISTS") || strings.Contains(lowerMsg, "already exists") {
		return insertErrorAlreadyExists
	}
	if strings.Contains(msg, "RESOURCE_NOT_FOUND") || strings.Contains(msg, "INVALID_ARGUMENT") || strings.Contains(lowerMsg, "forbidden") || strings.Contains(lowerMsg, "permission") || strings.Contains(lowerMsg, "invalid") {
		return insertErrorFatal
	}
	return insertErrorRetryable
}
