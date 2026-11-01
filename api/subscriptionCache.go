package api

import (
	"github.com/gin-gonic/gin"
	"github.com/shenaba/2s-ui/service"
)

func (*ApiService) GetExternalSubCache(c *gin.Context) {
	var cache service.ExternalSubCacheService
	config, err := cache.Config()
	if err != nil {
		jsonMsg(c, "", err)
		return
	}
	status, err := cache.Status()
	jsonObj(c, gin.H{"config": config, "sources": status}, err)
}

func (*ApiService) ConfigureExternalSubCache(c *gin.Context, actor string) {
	var config service.ExternalSubCacheConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		jsonMsg(c, "", err)
		return
	}
	var cache service.ExternalSubCacheService
	jsonMsg(c, "save", cache.Configure(config, actor))
}

func (*ApiService) RefreshExternalSubs(c *gin.Context) {
	var request struct {
		Keys   []string `json:"keys"`
		Resume bool     `json:"resume"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		jsonMsg(c, "", err)
		return
	}
	var cache service.ExternalSubCacheService
	jsonMsg(c, "", cache.Queue(request.Keys, request.Resume))
}
