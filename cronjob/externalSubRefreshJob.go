package cronjob

import (
	"context"
	"time"

	"github.com/shenaba/2s-ui/logger"
	"github.com/shenaba/2s-ui/service"
)

type ExternalSubRefreshJob struct{}

func (*ExternalSubRefreshJob) Run() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var cache service.ExternalSubCacheService
	if err := cache.RefreshOne(ctx); err != nil {
		// Database/configuration errors only. Provider errors are sanitized and
		// recorded in source status; URLs containing tokens are never logged.
		logger.Warning("external subscription refresh could not complete")
	}
}
