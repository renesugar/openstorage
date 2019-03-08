/*
Package storagepolicy manages storage policy and apply/validate storage policy restriction
volume operations.
Copyright 2018 Portworx

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

package storagepolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golang/protobuf/jsonpb"
	"github.com/portworx/kvdb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/libopenstorage/openstorage/api"
)

// SdkPolicyManager is an implementation of the
// Storage Policy Manager for the SDK
type SdkPolicyManager struct {
	kv kvdb.Kvdb
}

const (
	policyPrefix = "storage/policy"
	policyPath   = "/policies"
	defaultPath  = "storage/policy/enforce"
)

var (
	// Check interface
	_ PolicyManager = &SdkPolicyManager{}

	inst *SdkPolicyManager
	Inst = func() (PolicyManager, error) {
		return policyInst()
	}
)

func Init(kv kvdb.Kvdb) (PolicyManager, error) {
	if inst != nil {
		return nil, fmt.Errorf("Policy Manager is already initialized")
	}
	if kv == nil {
		return nil, fmt.Errorf("KVDB is not yet initialized.  " +
			"A valid KVDB instance required for the Storage Policy.")
	}

	inst = &SdkPolicyManager{
		kv: kv,
	}

	// Convert existing storagePolicy to new StoragePolicy struct,
	// may be need to move this to indivisual functions
	// Set no authentication so that we can override existing volumeSpecs with StoragePolicy
	err := volSpecToSdkStoragePolicy(inst)
	if err != nil {
		return nil, err
	}

	return inst, nil
}

func policyInst() (PolicyManager, error) {
	if inst == nil {
		return nil, fmt.Errorf("Policy Manager is not initialized")
	}
	return inst, nil
}

// Simple function which creates key for Kvdb
func prefixWithName(name string) string {
	return policyPrefix + policyPath + "/" + name
}

// Create Storage policy
func (p *SdkPolicyManager) Create(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyCreateRequest,
) (*api.SdkOpenStoragePolicyCreateResponse, error) {
	if req.StoragePolicy.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Must supply a Storage Policy Name")
	} else if req.StoragePolicy.GetPolicy() == nil {
		return nil, status.Error(codes.InvalidArgument, "Must supply Volume Specs")
	}

	// Add ownership details to storage policy
	// what does OwenershipSetUserNameFromContext do??
	req.StoragePolicy.Ownership = api.OwnershipSetUsernameFromContext(ctx, req.StoragePolicy.GetOwnership())

	// Since VolumeSpecPolicy has oneof method of proto,
	// we need to marshal it into string using protobuf jsonpb
	m := jsonpb.Marshaler{OrigName: true}
	policyStr, err := m.MarshalToString(req.GetStoragePolicy())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Json Marshal failed for policy %s: %v", req.StoragePolicy.GetName(), err)
	}

	_, err = p.kv.Create(prefixWithName(req.StoragePolicy.GetName()), policyStr, 0)
	if err == kvdb.ErrExist {
		return nil, status.Errorf(codes.AlreadyExists, "Storage Policy already exist : %v", req.StoragePolicy.GetName())
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to save storage policy: %v", err)
	}

	return &api.SdkOpenStoragePolicyCreateResponse{}, nil
}

// Update Storage policy
func (p *SdkPolicyManager) Update(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyUpdateRequest,
) (*api.SdkOpenStoragePolicyUpdateResponse, error) {
	if req.StoragePolicy.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Must supply a Storage Policy Name")
	}

	if req.StoragePolicy.GetPolicy() == nil {
		return nil, status.Error(codes.InvalidArgument, "Must supply Volume Specs")
	}

	// Get Existing details to merge
	_, err := p.Inspect(ctx,
		&api.SdkOpenStoragePolicyInspectRequest{
			Name: req.StoragePolicy.GetName(),
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Policy with name %s not found: %v", req.StoragePolicy.GetName(), err)
	}

	m := jsonpb.Marshaler{OrigName: true}
	updateStr, err := m.MarshalToString(req.GetStoragePolicy())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Json Marshal failed for policy %s: %v", req.StoragePolicy.GetName(), err)
	}

	if !req.GetStoragePolicy().IsPermitted(ctx, api.Ownership_Write) {
		return nil, status.Errorf(codes.PermissionDenied, "Cannot update storage policy")
	}
	_, err = p.kv.Update(prefixWithName(req.StoragePolicy.GetName()), updateStr, 0)
	if err == kvdb.ErrNotFound {
		return nil, status.Errorf(codes.NotFound, "Storage Policy %s not found", req.StoragePolicy.GetPolicy())
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to update storage policy: %v", err)
	}

	return &api.SdkOpenStoragePolicyUpdateResponse{}, nil
}

// Delete storage policy specified by name
func (p *SdkPolicyManager) Delete(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyDeleteRequest,
) (*api.SdkOpenStoragePolicyDeleteResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Must supply a Storage Policy Name")
	}

	// retrive default storage policy details
	inspResp, err := p.Inspect(ctx,
		&api.SdkOpenStoragePolicyInspectRequest{
			Name: req.GetName(),
		},
	)
	if err != nil {
		return &api.SdkOpenStoragePolicyDeleteResponse{}, nil
	}

	// release default policy restriction before deleting policy
	policy, err := p.DefaultInspect(ctx, &api.SdkOpenStoragePolicyDefaultInspectRequest{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Unable to retrive default policy details %v", err)
	}

	if policy.GetStoragePolicy() != nil && policy.GetStoragePolicy().GetName() == req.GetName() {
		_, err := p.Release(ctx, &api.SdkOpenStoragePolicyReleaseRequest{})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Removal of default policy failed with: %v", err)
		}
	}

	// Only the owner or the admin can delete
	if !inspResp.GetStoragePolicy().IsPermitted(ctx, api.Ownership_Admin) {
		return nil, status.Errorf(codes.PermissionDenied, "Cannot delete storage policy %v", req.GetName())
	}

	_, err = p.kv.Delete(prefixWithName(req.GetName()))
	if err != kvdb.ErrNotFound && err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to delete Storage Policy %s: %v", req.GetName(), err)
	}

	return &api.SdkOpenStoragePolicyDeleteResponse{}, nil
}

// Inspect storage policy specifed by name
func (p *SdkPolicyManager) Inspect(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyInspectRequest,
) (*api.SdkOpenStoragePolicyInspectResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Must supply a Storage Policy Name")
	}

	kvp, err := p.kv.Get(prefixWithName(req.GetName()))
	if err == kvdb.ErrNotFound {
		return nil, status.Errorf(codes.NotFound, "Policy %s not found", req.GetName())
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get policy %s information: %v", req.GetName(), err)
	}

	storPolicy := &api.SdkStoragePolicy{}
	err = jsonpb.UnmarshalString(string(kvp.Value), storPolicy)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Json Unmarshal failed for policy %s: %v", req.GetName(), err)
	}

	return &api.SdkOpenStoragePolicyInspectResponse{
		StoragePolicy: storPolicy,
	}, nil
}

// Enumerate all of storage policies
func (p *SdkPolicyManager) Enumerate(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyEnumerateRequest,
) (*api.SdkOpenStoragePolicyEnumerateResponse, error) {
	// get all keyValue pair at /storage/policy/policies
	kvp, err := p.kv.Enumerate(policyPrefix + policyPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get policies from database: %v", err)
	}

	policies := make([]*api.SdkStoragePolicy, 0)
	for _, policy := range kvp {
		sdkPolicy := &api.SdkStoragePolicy{}
		err = jsonpb.UnmarshalString(string(policy.Value), sdkPolicy)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Json Unmarshal failed for policy %s: %v", policy.Key, err)
		}
		policies = append(policies, sdkPolicy)
	}

	return &api.SdkOpenStoragePolicyEnumerateResponse{
		StoragePolicies: policies,
	}, nil
}

// SetDefault  storage policy
func (p *SdkPolicyManager) SetDefault(
	ctx context.Context,
	req *api.SdkOpenStoragePolicySetDefaultRequest,
) (*api.SdkOpenStoragePolicySetDefaultResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Must supply a Storage Policy Name")
	}

	// verify policy exists, before enforcing
	policy, err := p.Inspect(ctx,
		&api.SdkOpenStoragePolicyInspectRequest{
			Name: req.GetName(),
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Policy with name %s not found", req.GetName())
	}

	// only administrator or owner can set policy as default
	// should we allow admin only access here?
	if !policy.GetStoragePolicy().IsPermitted(ctx, api.Ownership_Admin) {
		return nil, status.Errorf(codes.PermissionDenied, "Cannot set storage policy as default %v", req.GetName())
	}

	policyStr, err := json.Marshal(policy.StoragePolicy.GetName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Json marshal failed for policy %s :%v", req.GetName(), err)
	}

	_, err = p.kv.Update(defaultPath, policyStr, 0)
	if err == kvdb.ErrNotFound {
		if _, err := p.kv.Create(defaultPath, policyStr, 0); err != nil {
			return nil, status.Errorf(codes.Internal, "Unable to save default policy details %v", err)
		}
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to set default policy: %v", err)
	}

	return &api.SdkOpenStoragePolicySetDefaultResponse{}, nil
}

// Release storage policy if set as default
func (p *SdkPolicyManager) Release(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyReleaseRequest,
) (*api.SdkOpenStoragePolicyReleaseResponse, error) {

	// only administrator or owner can remove storage policy restriction
	// should we allow admin only access here?
	// if !req.GetStoragePolicy().IsPermitted(ctx, api.Ownership_Admin) {
	// 	return nil, status.Errorf(codes.PermissionDenied, "Cannot set remove storage policy restriction")
	// }
	// // empty represents no policy is set as default
	strB, _ := json.Marshal("")
	_, err := p.kv.Update(defaultPath, strB, 0)
	if err != kvdb.ErrNotFound && err != nil {
		return nil, status.Errorf(codes.Internal, "Remove storage policy restriction failed with: %v", err)
	}

	return &api.SdkOpenStoragePolicyReleaseResponse{}, nil
}

// DefaultInspect return default storeage policy details
func (p *SdkPolicyManager) DefaultInspect(
	ctx context.Context,
	req *api.SdkOpenStoragePolicyDefaultInspectRequest,
) (*api.SdkOpenStoragePolicyDefaultInspectResponse, error) {
	var policyName string
	defaultPolicy := &api.SdkOpenStoragePolicyDefaultInspectResponse{}

	_, err := p.kv.GetVal(defaultPath, &policyName)
	// defaultPath key is not created
	if err == kvdb.ErrNotFound {
		return defaultPolicy, nil
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Unable to retrive default policy details: %v", err)
	}

	// no default policy found
	if policyName == "" {
		return defaultPolicy, nil
	}

	// retrive default storage policy details
	inspResp, err := p.Inspect(context.Background(),
		&api.SdkOpenStoragePolicyInspectRequest{
			Name: policyName,
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Policy with name %s not found", policyName)
	}

	return &api.SdkOpenStoragePolicyDefaultInspectResponse{
		StoragePolicy: inspResp.GetStoragePolicy(),
	}, nil
}

func volSpecToSdkStoragePolicy(inst *SdkPolicyManager) error {
	kvp, err := inst.kv.Enumerate(policyPrefix + policyPath)
	if err == kvdb.ErrNotFound {
		return nil
	} else if err != nil {
		return status.Errorf(codes.Internal, "Failed to get existing policies from database: %v", err)
	}

	for _, policy := range kvp {
		volSpecs := &api.VolumeSpecPolicy{}
		err = jsonpb.UnmarshalString(string(policy.Value), volSpecs)
		if err != nil {
			return status.Errorf(codes.Internal, "Json Unmarshal failed for policy %s: %v", policy.Key, err)
		}
		storagePolicy := &api.SdkStoragePolicy{
			Name:   strings.TrimPrefix(policy.Key, policyPrefix+policyPath+"/"),
			Policy: volSpecs,
		}

		_, err = inst.Update(context.Background(), &api.SdkOpenStoragePolicyUpdateRequest{
			StoragePolicy: storagePolicy,
		})
		if err != nil {
			return fmt.Errorf("Storage Policy init failed %v", err)
		}
	}
	return nil
}
