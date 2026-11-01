package api

import (
	"github.com/gin-gonic/gin"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/service"
)

func (*ApiService) GetSubscriptionDistribution(c *gin.Context) {
	var templates service.SubscriptionTemplateService
	entries, err := templates.List()
	if err != nil {
		jsonMsg(c, "", err)
		return
	}
	var aliases service.LegacySubscriptionAliasService
	legacy, err := aliases.List()
	if err != nil {
		jsonMsg(c, "", err)
		return
	}
	clients := []service.ExternalSubCatalogClient{}
	err = database.GetDB().Model(&model.Client{}).Select("id,name,enable,expiry").Order("name,id").Scan(&clients).Error
	jsonObj(c, gin.H{"templates": entries, "aliases": legacy, "clients": clients}, err)
}
func (*ApiService) GetSubscriptionTemplate(c *gin.Context) {
	var templates service.SubscriptionTemplateService
	entry, err := templates.Get(c.Query("id"))
	if err != nil {
		jsonMsg(c, "", err)
		return
	}
	jsonObj(c, gin.H{"id": entry.Id, "revision": entry.Revision, "content": entry.Content}, nil)
}
func (*ApiService) SaveSubscriptionTemplate(c *gin.Context, actor string) {
	var input service.SubscriptionTemplateWrite
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "", err)
		return
	}
	var templates service.SubscriptionTemplateService
	id, err := templates.Save(input, actor)
	jsonMsgObj(c, "save", gin.H{"id": id}, err)
}
func (*ApiService) DeleteSubscriptionTemplate(c *gin.Context, actor string) {
	var input struct {
		Id       string `json:"id"`
		Revision uint64 `json:"revision"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "", err)
		return
	}
	var templates service.SubscriptionTemplateService
	jsonMsg(c, "", templates.Delete(input.Id, input.Revision, actor))
}
func (*ApiService) SaveLegacySubscriptionAlias(c *gin.Context, actor string) {
	var input service.LegacySubscriptionAliasWrite
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "", err)
		return
	}
	var aliases service.LegacySubscriptionAliasService
	id, err := aliases.Save(input, actor)
	jsonMsgObj(c, "save", gin.H{"id": id}, err)
}
func (*ApiService) DeleteLegacySubscriptionAlias(c *gin.Context, actor string) {
	var input struct {
		Id       string `json:"id"`
		Revision uint64 `json:"revision"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "", err)
		return
	}
	var aliases service.LegacySubscriptionAliasService
	jsonMsg(c, "", aliases.Delete(input.Id, input.Revision, actor))
}
