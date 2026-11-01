package sub

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shenaba/2s-ui/service"
)

func (s *SubHandler) sourceSnapshot(c *gin.Context, head bool) {
	c.Header("Cache-Control", "no-store")
	// Do not distinguish an unknown bearer from another reader's source.
	deny := func() { c.Status(http.StatusNotFound) }
	query := c.Request.URL.Query()
	if len(query["source"]) != 1 || len(query["format"]) > 1 {
		deny()
		return
	}
	format := c.Query("format")
	native := false
	switch format {
	case "clash":
		native = true
	case "universal":
	case "":
		ua := strings.ToLower(c.GetHeader("User-Agent"))
		native = strings.Contains(ua, "clash") || strings.Contains(ua, "mihomo")
	default:
		deny()
		return
	}
	var sources service.ExternalSubSourceService
	snapshot, err := sources.SnapshotForReader(c.Query("source"), c.Param("subid"), native)
	if err != nil {
		if errors.Is(err, service.ErrExternalSubCacheUnavailable) {
			c.Header("Retry-After", "300")
			c.Status(http.StatusServiceUnavailable)
		} else {
			deny()
		}
		return
	}
	// Use the provider snapshot's time and quota, never the feed account's
	// creation time/quota or the time this cached payload was downloaded.
	c.Header("Last-Modified", time.Unix(snapshot.UpdatedAt, 0).UTC().Format(http.TimeFormat))
	c.Header("X-Snapshot-Updated-At", strconv.FormatInt(snapshot.UpdatedAt, 10))
	var fields []string
	for _, key := range []string{"upload", "download", "total", "expire"} {
		if value, ok := snapshot.UserInfo[key]; ok {
			fields = append(fields, key+"="+strconv.FormatUint(value, 10))
		}
	}
	if len(fields) > 0 {
		c.Header("Subscription-Userinfo", strings.Join(fields, "; "))
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	if head {
		c.Status(http.StatusOK)
		return
	}
	c.String(http.StatusOK, "%s", snapshot.Body)
}
