package main

import (
	"os"

	"github.com/gin-gonic/gin"
)

func main() {
	e := gin.New()

	e.GET("/Generate", SlashFunction)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	err := e.Run(":" + port)
	if err != nil {
		panic(err.Error())
	}
}
