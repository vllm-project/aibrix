/*
Copyright 2025 The Aibrix Team.

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

package handler

import (
	"context"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

type JobHandler struct {
	pb.UnimplementedJobServiceServer
	store store.Store
}

func NewJobHandler(s store.Store) *JobHandler {
	return &JobHandler{store: s}
}

func (h *JobHandler) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	jobs, err := h.store.ListJobs(ctx, req.Search, req.Status)
	if err != nil {
		return nil, err
	}
	return &pb.ListJobsResponse{Jobs: jobs}, nil
}

func (h *JobHandler) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	return h.store.GetJob(ctx, req.Id)
}

func (h *JobHandler) CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.Job, error) {
	return h.store.CreateJob(ctx, req)
}
