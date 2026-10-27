package catalog

import (
	"github.com/gofiber/fiber/v2"
	"github.com/labstack/echo/v4"
)

func Echo() {
	e := echo.New()
	g := e.Group("/catalog")
	g.GET("/items/:id", getItem)
	e.DELETE("/catalog/items/:id", getItem)
}

func Fiber() {
	app := fiber.New()
	v1 := app.Group("/f/v1")
	v1.Get("/search", search)
	app.Post("/f/upload", search)
}

func getItem(c echo.Context) error { return nil }
func search(c *fiber.Ctx) error    { return nil }
