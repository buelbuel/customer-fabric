package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/browser"
)

func main() {
	if os.Getenv("CF_MODE") != "development" {
		log.Fatal("browser reference requires development mode")
	}
	handler, err := browser.New(os.Getenv("CF_REFERENCE_KRATOS"), os.Getenv("CF_REFERENCE_API"), os.Getenv("CF_REFERENCE_COOKIE"))
	if err != nil {
		log.Fatal("invalid browser reference configuration")
	}
	server := &http.Server{
		Addr: ":8081", Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("browser reference server failed")
	}
}
