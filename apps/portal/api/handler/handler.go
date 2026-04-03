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

package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Handler struct {
	client client.Client
}

func New(c client.Client) *Handler {
	return &Handler{client: c}
}

func parsePagination(c *gin.Context) types.PaginationParams {
	p := types.DefaultPagination()
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Page = n
		}
	}
	if v := c.Query("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			p.PageSize = n
		}
	}
	return p
}

func respondError(c *gin.Context, code int, message, reason string) {
	c.JSON(code, types.ErrorResponse{
		Error: types.ErrorDetail{
			Code:    code,
			Message: message,
			Reason:  reason,
		},
	})
}

func namespaceFilter(c *gin.Context) string {
	return c.Query("namespace")
}
