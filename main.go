package main

import (
	"net/http"
	"os"
)

func main() {
	router := http.NewServeMux()
	router.HandleFunc("/generate", SlashFunction)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	err := http.ListenAndServe(":"+port, router)
	if err != nil {
		panic(err.Error())
	}
}
