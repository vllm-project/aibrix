/*
Copyright 2024 The Aibrix Team.

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

package gateway

import (
	"context"
	"fmt"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) checkLimits(ctx context.Context, user utils.User) (int64, *extProcPb.ProcessingResponse, error) {
	if user.Rpm == 0 {
		user.Rpm = int64(DefaultRPM)
	}
	if user.Tpm == 0 {
		user.Tpm = user.Rpm * int64(DefaultTPMMultiplier)
	}

	code, err := s.checkRPM(ctx, user.Name, user.Rpm)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorRPMExceeded, RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	rpm, code, err := s.incrRPM(ctx, user.Name)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorIncrRPM, RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	code, err = s.checkTPM(ctx, user.Name, user.Tpm)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorTPMExceeded, RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	return rpm, nil, nil
}

func (s *Server) checkRPM(ctx context.Context, username string, rpmLimit int64) (envoyTypePb.StatusCode, error) {
	rpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_RPM_CURRENT", username))
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get RPM for user: %v", username)
	}

	if rpmCurrent >= rpmLimit {
		return envoyTypePb.StatusCode_TooManyRequests, fmt.Errorf("user: %v has exceeded RPM: %v", username, rpmLimit)
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) incrRPM(ctx context.Context, username string) (int64, envoyTypePb.StatusCode, error) {
	rpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_RPM_CURRENT", username), 1)
	if err != nil {
		return rpm, envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to increment RPM for user: %v", username)
	}

	return rpm, envoyTypePb.StatusCode_OK, nil
}

func (s *Server) checkTPM(ctx context.Context, username string, tpmLimit int64) (envoyTypePb.StatusCode, error) {
	tpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_TPM_CURRENT", username))
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get TPM for user: %v", username)
	}

	if tpmCurrent >= tpmLimit {
		return envoyTypePb.StatusCode_TooManyRequests, fmt.Errorf("user: %v has exceeded TPM: %v", username, tpmLimit)
	}

	return envoyTypePb.StatusCode_OK, nil
}
