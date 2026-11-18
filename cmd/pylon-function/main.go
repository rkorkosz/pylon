// Command pylon-function runs the Functions Framework on loopback for local checks.
package main

import (
	"log"
	"os"

	"github.com/GoogleCloudPlatform/functions-framework-go/funcframework"
	_ "github.com/rkrokosz/pylon" // Register SyncInvoices.
)

func main() {
	if err := os.Setenv("FUNCTION_TARGET", "SyncInvoices"); err != nil {
		log.Fatal(err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(funcframework.StartHostPort("127.0.0.1", port))
}
