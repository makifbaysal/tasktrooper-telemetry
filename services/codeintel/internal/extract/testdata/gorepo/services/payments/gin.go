package payments

import "github.com/gin-gonic/gin"

func Routes() *gin.Engine {
	r := gin.New()
	api := r.Group("/api/v1")
	api.POST("/charges", h.createCharge)
	api.GET("/charges/:id", h.getCharge)
	registerRefunds(api)
	r.GET("/healthz", func(c *gin.Context) {})
	return r
}

func registerRefunds(g *gin.RouterGroup) {
	g.POST("/refunds", refund)
}

func refund(c *gin.Context) {}

var h = &Handler{}

type Handler struct{}

func (h *Handler) createCharge(c *gin.Context) { chargeCard() }
func (h *Handler) getCharge(c *gin.Context)    {}
func chargeCard() error                        { return nil }
