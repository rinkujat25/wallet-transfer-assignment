package middleware

import "github.com/gofiber/fiber/v3"

// Register wires global middleware for the application.
func Register(app *fiber.App) {
	RegisterWithConfig(app)
}

// RegisterWithConfig wires global middleware.
func RegisterWithConfig(app *fiber.App) {
	app.Use(JSONContentType)
}

// JSONContentType sets the default response content type.
func JSONContentType(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Next()
}
