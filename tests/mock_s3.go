package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("MockS3: Received %s %s", r.Method, r.URL.Path)

		// Resilience Test: Simulate 404 for specific key
		// The integration test expects /test-bucket/missing.jpg to return 404
		if strings.Contains(r.URL.Path, "missing.jpg") || strings.Contains(r.URL.Path, "non-existent") {
			log.Printf("MockS3: Returning 404 for %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("404 Not Found"))
			return
		}

		// Cache Purging Test: Handle DELETE
		if r.Method == http.MethodDelete {
			log.Printf("MockS3: Returning 204 for DELETE %s", r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Standard GET: Serve the test image
		// We expect the test runner to create 'tests/test_image.jpg'
		f, err := os.Open("tests/test_image.jpg")
		if err != nil {
			log.Printf("MockS3: Failed to open test image: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "Error opening test image: %v", err)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("ETag", "\"mock-etag-123\"")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")

		if _, err := io.Copy(w, f); err != nil {
			log.Printf("MockS3: Error sending file: %v", err)
		}
	})

	log.Printf("MockS3 listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
