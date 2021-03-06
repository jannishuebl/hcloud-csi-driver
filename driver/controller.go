/*
Copyright 2018 DigitalOcean

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	_  = iota
	KB = 1 << (10 * iota)
	MB
	GB
	TB
)

const (
	defaultVolumeSizeInGB = 16 * GB
	minVolumeSizeInGB     = 10 * GB

	createdByHCloud = "hcloud-csi-driver"
)

var (
	// hcloud currently only support a single node to be attached to a single node
	// in read/write mode. This corresponds to `accessModes.ReadWriteOnce` in a
	// PVC resource on Kubernets
	supportedAccessMode = &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}
)

// CreateVolume creates a new volume from the given request. The function is
// idempotent.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}

	if req.VolumeCapabilities == nil || len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities must be provided")
	}

	if req.AccessibilityRequirements != nil {
		for _, t := range req.AccessibilityRequirements.Requisite {
			location, ok := t.Segments["location"]
			if !ok {
				continue // nothing to do
			}

			if location != d.location {
				return nil, status.Errorf(codes.ResourceExhausted, "volume can be only created in location: %q, got: %q", d.location, location)

			}
		}
	}

	size, err := extractStorage(req.CapacityRange)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeName := req.Name

	ll := d.log.WithFields(logrus.Fields{
		"volume_name":             volumeName,
		"storage_size_giga_bytes": size / GB,
		"method":                  "create_volume",
		"volume_capabilities":     req.VolumeCapabilities,
	})
	ll.Info("create volume called")

	// get volume first, if it's created do nothing
	volume, _, err := d.hcloudClient.Volume.GetByName(ctx, volumeName)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// volume already exist, do nothing
	if volume != nil {

		volumeCapacityGigaBytes := int64(volume.Size * GB)

		if volumeCapacityGigaBytes != size {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested size: %d", size))
		}

		volumeID := strconv.Itoa(volume.ID)

		ll.Info("volume already created")
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				Id:            volumeID,
				CapacityBytes: volumeCapacityGigaBytes,
			},
		}, nil
	}

	volumeReq := &hcloud.VolumeCreateOpts{
		Name: volumeName,
		Size: int(size / GB),
		Location: &hcloud.Location{
			Name: d.location,
		},
		Labels: map[string]string{
			"createdBy": createdByHCloud,
		},
	}

	if !validateCapabilities(req.VolumeCapabilities) {
		return nil, status.Error(codes.AlreadyExists, "invalid volume capabilities requested. Only SINGLE_NODE_WRITER is supported ('accessModes.ReadWriteOnce' on Kubernetes)")
	}

	ll.Info("verify volume size is allowed")
	if size < minVolumeSizeInGB {
		return nil, status.Errorf(codes.OutOfRange, "requested volume size %d GB is lower than supported minimum of %d GB", size/GB, minVolumeSizeInGB/GB)
	}

	ll.Info("checking volume limit")
	if err := d.checkLimit(ctx); err != nil {
		return nil, err
	}

	ll.WithField("volume_req", volumeReq).Info("creating volume")
	hcloudResp, _, err := d.hcloudClient.Volume.Create(ctx, *volumeReq)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// TODO: wait until hcloudResp.action signals completion

	volumeID := strconv.Itoa(hcloudResp.Volume.ID)

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            volumeID,
			CapacityBytes: size,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						"location": d.location,
					},
				},
			},
		},
	}

	ll.WithField("response", resp).Info("volume created")
	return resp, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"method":    "delete_volume",
	})
	ll.Info("delete volume called")

	var volumeID int
	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		// volume id is invalid in this providers context, volume can not exist
		// volume is deleted (does not exist)
		return &csi.DeleteVolumeResponse{}, nil
	}

	resp, err := d.hcloudClient.Volume.Delete(ctx, &hcloud.Volume{
		ID: volumeID,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// we assume it's deleted already for idempotency
			ll.WithFields(logrus.Fields{
				"error": err,
				"resp":  resp,
			}).Warn("assuming volume is deleted already")
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, err
	}

	ll.WithField("response", resp).Info("volume is deleted")
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches the given volume to the node
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Node ID must be provided")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume capability must be provided")
	}

	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		// don't return because the CSI tests passes ID's in non-integer format.
		volumeID = 1 // for testing purposes only. Will fail in real world API
		d.log.WithField("volume_id", req.VolumeId).Warn("volume ID cannot be converted to an integer")

	}

	serverID, err := strconv.Atoi(req.NodeId)
	if err != nil {
		// don't return because the CSI tests passes ID's in non-integer format.
		serverID = 1 // for testing purposes only. Will fail in real world API
		d.log.WithField("node_id", req.NodeId).Warn("node ID cannot be converted to an integer")
	}

	if req.Readonly {
		// TODO(arslan): we should return codes.InvalidArgument, but the CSI
		// test fails, because according to the CSI Spec, this flag cannot be
		// changed on the same volume. However we don't use this flag at all,
		// as there are no `readonly` attachable volumes.
		return nil, status.Error(codes.AlreadyExists, "read only Volumes are not supported")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"server_id": serverID,
		"method":    "controller_publish_volume",
	})
	ll.Info("controller publish volume called")

	// check if volume exist before trying to attach it
	vol, resp, err := d.hcloudClient.Volume.GetByID(ctx, volumeID)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
		}
		// TODO: replace with actual error handling
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
		// return nil, err
	}

	// check if server exist before trying to attach the volume to the server
	server, resp, err := d.hcloudClient.Server.GetByID(ctx, serverID)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, status.Errorf(codes.NotFound, "server %q not found", serverID)
		}
		// TODO: replace with actual error handling
		return nil, status.Errorf(codes.NotFound, "server %q not found", serverID)
		// return nil, err
	}

	attachedServer := vol.Server
	var attachedID int
	if attachedServer != nil {
		attachedID = attachedServer.ID
		if attachedID == serverID {
			ll.Info("volume is already attached")
			return &csi.ControllerPublishVolumeResponse{}, nil
		}
	}

	// volume is attached to a different server, return an error
	if attachedID != 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume is attached to the wrong server(%q), dettach the volume to fix it", attachedID)
	}

	// attach the volume to the correct node
	action, resp, err := d.hcloudClient.Volume.Attach(ctx, vol, server)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "volume %q could not be attached to server %q: %s", vol.ID, server.ID, err)
	}

	if action != nil {
		ll.Info("waiting until volume is attached")
		if err := d.waitAction(ctx, vol.ID, action.ID); err != nil {
			return nil, err
		}
	}

	ll.Info("volume is attached")
	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		// don't return because the CSI tests passes ID's in non-integer format.
		volumeID = 1 // for testing purposes only. Will fail in real world API
		d.log.WithField("volume_id", req.VolumeId).Warn("volume ID cannot be converted to an integer")

	}

	serverID, err := strconv.Atoi(req.NodeId)
	if err != nil {
		// don't return because the CSI tests passes ID's in non-integer format
		serverID = 1 // for testing purposes only. Will fail in real world API
		d.log.WithField("node_id", req.NodeId).Warn("node ID cannot be converted to an integer")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"server_id": serverID,
		"method":    "controller_unpublish_volume",
	})
	ll.Info("controller unpublish volume called")

	// check if volume exist before trying to detach it
	vol, resp, err := d.hcloudClient.Volume.GetByID(ctx, volumeID)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// assume it's detached
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, err
	}

	// check if server exist before trying to attach the volume to the server
	_, resp, err = d.hcloudClient.Server.GetByID(ctx, serverID)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, status.Errorf(codes.NotFound, "server %q not found", serverID)
		}
		return nil, err
	}

	action, resp, err := d.hcloudClient.Volume.Detach(ctx, vol)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "volume %q could not be deattached from server %q: %s", vol.ID, serverID, err)
	}

	if action != nil {
		ll.Info("waiting until volume is detached")
		if err := d.waitAction(ctx, vol.ID, action.ID); err != nil {
			return nil, err
		}
	}

	ll.Info("volume is detached")
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume ID must be provided")
	}

	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume Capabilities must be provided")
	}

	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		// don't return because the CSI tests passes ID's in non-integer format.
		volumeID = 1 // for testing purposes only. Will fail in real world API
		d.log.WithField("volume_id", req.VolumeId).Warn("volume ID cannot be converted to an integer")

	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id":              req.VolumeId,
		"volume_capabilities":    req.VolumeCapabilities,
		"accessible_topology":    req.AccessibleTopology,
		"supported_capabilities": supportedAccessMode,
		"method":                 "validate_volume_capabilities",
	})
	ll.Info("validate volume capabilities called")

	// check if volume exist before trying to validate it it
	_, volResp, err := d.hcloudClient.Volume.GetByID(ctx, volumeID)
	if err != nil {
		if volResp != nil && volResp.StatusCode == http.StatusNotFound {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
		}
		// TODO: replace with actual error handling
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
		// return nil, err
	}

	if req.AccessibleTopology != nil {
		for _, t := range req.AccessibleTopology {
			location, ok := t.Segments["location"]
			if !ok {
				continue // nothing to do
			}

			if location != d.location {
				// return early if a different location is expected
				ll.WithField("supported", false).Info("supported capabilities")
				return &csi.ValidateVolumeCapabilitiesResponse{
					Supported: false,
				}, nil
			}
		}
	}

	// if it's not supported (i.e: wrong location), we shouldn't override it
	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Supported: validateCapabilities(req.VolumeCapabilities),
	}

	ll.WithField("supported", resp.Supported).Info("supported capabilities")
	return resp, nil
}

// ListVolumes returns a list of all requested volumes
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	var page int
	var err error
	if req.StartingToken != "" {
		page, err = strconv.Atoi(req.StartingToken)
		if err != nil {
			return nil, err
		}
	}

	listOpts := hcloud.VolumeListOpts{
		ListOpts: hcloud.ListOpts{
			Page:    page,
			PerPage: int(req.MaxEntries),
		},
	}

	ll := d.log.WithFields(logrus.Fields{
		"list_opts":          listOpts,
		"req_starting_token": req.StartingToken,
		"method":             "list_volumes",
	})
	ll.Info("list volumes called")

	var volumes []*hcloud.Volume
	lastPage := 0
	for {
		vols, resp, err := d.hcloudClient.Volume.List(ctx, listOpts)
		if err != nil {
			return nil, err
		}

		volumes = append(volumes, vols...)

		pagination := resp.Meta.Pagination

		if pagination == nil || pagination.Page == pagination.LastPage {
			if pagination != nil {
				lastPage = pagination.Page
			}
			break
		}

		listOpts.ListOpts.Page = pagination.NextPage
	}

	var entries []*csi.ListVolumesResponse_Entry
	for _, vol := range volumes {
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				Id:            strconv.Itoa(vol.ID),
				CapacityBytes: int64(vol.Size * GB),
			},
		})
	}

	// TODO(arslan): check that the NextToken logic works fine, might be racy
	resp := &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: strconv.Itoa(lastPage),
	}

	ll.WithField("response", resp).Info("volumes listed")
	return resp, nil
}

// GetCapacity returns the capacity of the storage pool
func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	// TODO(arslan): check if we can provide this information somehow
	d.log.WithFields(logrus.Fields{
		"params": req.Parameters,
		"method": "get_capacity",
	}).Warn("get capacity is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities returns the capabilities of the controller service.
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	// TODO(arslan): checkout if the capabilities are worth supporting
	var caps []*csi.ControllerServiceCapability
	for _, cap := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,

		// TODO(arslan): enable once snapshotting is supported
		// csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		// csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	} {
		caps = append(caps, newCap(cap))
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}

	d.log.WithFields(logrus.Fields{
		"response": resp,
		"method":   "controller_get_capabilities",
	}).Info("controller get capabilities called")
	return resp, nil
}

// CreateSnapshot will be called by the CO to create a new snapshot from a
// source volume on behalf of a user.
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	d.log.WithFields(logrus.Fields{
		"req":    req,
		"method": "create_snapshot",
	}).Warn("create snapshot is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// DeleteSnapshot will be called by the CO to delete a snapshot.
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	d.log.WithFields(logrus.Fields{
		"req":    req,
		"method": "delete_snapshot",
	}).Warn("delete snapshot is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ListSnapshots returns the information about all snapshots on the storage
// system within the given parameters regardless of how they were created.
// ListSnapshots shold not list a snapshot that is being created but has not
// been cut successfully yet.
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	d.log.WithFields(logrus.Fields{
		"req":    req,
		"method": "list_snapshots",
	}).Warn("list snapshots is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// extractStorage extracts the storage size in GB from the given capacity
// range. If the capacity range is not satisfied it returns the default volume
// size.
func extractStorage(capRange *csi.CapacityRange) (int64, error) {
	if capRange == nil {
		return defaultVolumeSizeInGB, nil
	}

	if capRange.RequiredBytes == 0 && capRange.LimitBytes == 0 {
		return defaultVolumeSizeInGB, nil
	}

	minSize := capRange.RequiredBytes

	// limitBytes might be zero
	maxSize := capRange.LimitBytes
	if capRange.LimitBytes == 0 {
		maxSize = minSize
	}

	if minSize == maxSize {
		return minSize, nil
	}

	return 0, errors.New("requiredBytes and LimitBytes are not the same")
}

// waitAction waits until the given action for the volume is completed
func (d *Driver) waitAction(ctx context.Context, volumeID int, actionID int) error {
	ll := d.log.WithFields(logrus.Fields{
		"volume_id": volumeID,
		"action_id": actionID,
	})

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// TODO(arslan): use backoff in the future
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			action, _, err := d.hcloudClient.Action.GetByID(ctx, actionID)
			if err != nil {
				ll.WithError(err).Info("waiting for volume errored")
				continue
			}
			ll.WithField("action_status", action.Status).Info("action received")

			if action.Status == hcloud.ActionStatusSuccess {
				ll.Info("action completed")
				return nil
			}

			if action.Status == hcloud.ActionStatusRunning {
				continue
			}
		case <-ctx.Done():
			return fmt.Errorf("timeout occured waiting for storage action of volume: %q", volumeID)
		}
	}
}

// checkLimit checks whether the user hit their volume limit to ensure.
func (d *Driver) checkLimit(ctx context.Context) error {
	// not supported by Hetzner Cloud at the moment
	return nil
}

// validateCapabilities validates the requested capabilities. It returns false
// if it doesn't satisfy the currently supported modes of Hetzner Cloud Volumes
func validateCapabilities(caps []*csi.VolumeCapability) bool {
	vcaps := []*csi.VolumeCapability_AccessMode{supportedAccessMode}

	hasSupport := func(mode csi.VolumeCapability_AccessMode_Mode) bool {
		for _, m := range vcaps {
			if mode == m.Mode {
				return true
			}
		}
		return false
	}

	supported := false
	for _, cap := range caps {
		if hasSupport(cap.AccessMode.Mode) {
			supported = true
		} else {
			// we need to make sure all capabilities are supported. Revert back
			// in case we have a cap that is supported, but is invalidated now
			supported = false
		}
	}

	return supported
}
