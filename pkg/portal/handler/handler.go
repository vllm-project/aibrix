package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/vllm-project/aibrix/pkg/portal/types"
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
