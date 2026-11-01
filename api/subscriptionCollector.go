package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shenaba/2s-ui/service"
)

// Machine routes use HMAC and source-IP authentication, independently of panel
// cookies or apiv2 administrator tokens. No collector registration defaults on.
func NewCollectorHandler(group *gin.RouterGroup, peer ...func(*gin.Context) string) {
	resolve := getRemoteIp
	if len(peer) > 0 && peer[0] != nil {
		resolve = peer[0]
	}
	group.POST("/tasks", func(c *gin.Context) { collectorRequest(c, false, resolve(c)) })
	group.POST("/results", func(c *gin.Context) { collectorRequest(c, true, resolve(c)) })
}
func collectorRequest(c *gin.Context, upload bool, peer string) {
	c.Header("Cache-Control", "no-store")
	var collector service.ExternalCollectorService
	limit := int64(4096)
	if upload {
		limit = 3 * 1024 * 1024
	}
	id, body, err := collector.Authenticate(c.Request, peer, limit)
	if err != nil {
		collectorError(c, err)
		return
	}
	if upload {
		var input service.ExternalCollectorUpload
		if json.Unmarshal(body, &input) != nil {
			collectorError(c, service.ErrCollectorInput)
			return
		}
		receipt, err := collector.Complete(id, input)
		if err != nil {
			collectorError(c, err)
			return
		}
		c.JSON(http.StatusOK, receipt)
	} else {
		var input map[string]interface{}
		if json.Unmarshal(body, &input) != nil || input == nil || len(input) > 0 {
			collectorError(c, service.ErrCollectorInput)
			return
		}
		tasks, err := collector.Claim(id)
		if err != nil {
			collectorError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"tasks": tasks, "nextPollSeconds": 60})
	}
}
func collectorError(c *gin.Context, err error) {
	status, code := http.StatusServiceUnavailable, "collector-request-failed"
	switch {
	case errors.Is(err, service.ErrCollectorUnauthorized):
		status, code = 401, "unauthorized"
	case errors.Is(err, service.ErrCollectorStale):
		status, code = 409, "stale-job"
	case errors.Is(err, service.ErrCollectorInput):
		status, code = 400, "invalid-request"
	case errors.Is(err, service.ErrCollectorBodySize):
		status, code = 413, "request-too-large"
	case errors.Is(err, service.ErrCollectorRate):
		status, code = 429, "rate-limited"
	}
	c.JSON(status, gin.H{"error": code})
}
