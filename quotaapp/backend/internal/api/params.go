package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func int64Param(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || v <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "非法路径参数 " + name})
		return 0, false
	}
	return v, true
}

func intParam(c *gin.Context, name string) (int, bool) {
	v, err := strconv.Atoi(c.Param(name))
	if err != nil || v <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "非法路径参数 " + name})
		return 0, false
	}
	return v, true
}

func int64Query(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Query(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}
