// main.go
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Service represents a microservice registered in the registry
type Service struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// RegistryClient simulates interaction with the Cdaprod Registry
type RegistryClient struct {
	RegistryURL string
	Client      *http.Client
}

// NewRegistryClient creates a new instance of RegistryClient
func NewRegistryClient(registryURL string) *RegistryClient {
	return &RegistryClient{
		RegistryURL: registryURL,
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// GetServices fetches the list of registered services from the registry
func (rc *RegistryClient) GetServices() ([]Service, error) {
	resp, err := rc.Client.Get(rc.RegistryURL + "/services")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("failed to fetch services from registry")
	}

	var services []Service
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		return nil, err
	}

	return services, nil
}

// Authentication Middleware
func authMiddleware(next http.Handler, validAPIKey string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != validAPIKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Logging Middleware
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		log.Printf("Started %s %s", r.Method, r.URL.Path)

		// Capture the response status
		rr := &responseRecorder{w, http.StatusOK}
		next.ServeHTTP(rr, r)

		duration := time.Since(startTime)
		log.Printf("Completed %d %s in %v", rr.statusCode, http.StatusText(rr.statusCode), duration)
	})
}

// responseRecorder is used to capture the status code for logging
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code
func (rr *responseRecorder) WriteHeader(code int) {
	rr.statusCode = code
	rr.ResponseWriter.WriteHeader(code)
}

// ServiceMeshHandler handles incoming requests and routes them to the appropriate service
type ServiceMeshHandler struct {
	Registry *RegistryClient
	Routes   map[string]string
	Client   *http.Client
}

// NewServiceMeshHandler creates a new ServiceMeshHandler
func NewServiceMeshHandler(registry *RegistryClient) *ServiceMeshHandler {
	return &ServiceMeshHandler{
		Registry: registry,
		Routes:   make(map[string]string),
		Client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// UpdateRoutes fetches the latest services from the registry and updates the routing table
func (sm *ServiceMeshHandler) UpdateRoutes() error {
	services, err := sm.Registry.GetServices()
	if err != nil {
		return err
	}

	newRoutes := make(map[string]string)
	for _, service := range services {
		newRoutes[service.Name] = service.URL
	}
	sm.Routes = newRoutes
	return nil
}

// ServeHTTP routes the request to the appropriate service based on the URL path
func (sm *ServiceMeshHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Expected URL pattern: /serviceName/optional/path
	pathParts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(pathParts) == 0 {
		http.Error(w, "Service name not specified", http.StatusBadRequest)
		return
	}

	serviceName := pathParts[0]
	targetURL, exists := sm.Routes[serviceName]
	if !exists {
		http.Error(w, "Service not found", http.StatusNotFound)
		return
	}

	// Construct the target URL
	var targetPath string
	if len(pathParts) > 1 {
		targetPath = pathParts[1]
	} else {
		targetPath = ""
	}

	fullURL := targetURL
	if targetPath != "" {
		fullURL = strings.TrimRight(targetURL, "/") + "/" + targetPath
	}

	// Forward the request
	sm.forwardRequest(w, r, fullURL)
}

// forwardRequest forwards the incoming request to the target service with retry logic
func (sm *ServiceMeshHandler) forwardRequest(w http.ResponseWriter, r *http.Request, target string) {
	// Create a new request
	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "Failed to create request to target service", http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Implement simple retry logic
	maxRetries := 3
	var resp *http.Response
	for attempt := 1; attempt <= maxRetries; attempt++ {
		resp, err = sm.Client.Do(req)
		if err == nil && resp.StatusCode < 500 {
			break
		}
		log.Printf("Attempt %d: Failed to forward request to %s: %v", attempt, target, err)
		time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
	}

	if err != nil {
		http.Error(w, "Failed to reach target service", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	// Copy the response back to the client
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func main() {
	// Configuration
	registryURL := "http://localhost:8081" // URL of the Cdaprod Registry
	apiKey := "your-secure-api-key"        // Replace with a secure API key

	// Initialize Registry Client
	registryClient := NewRegistryClient(registryURL)

	// Initialize Service Mesh Handler
	serviceMesh := NewServiceMeshHandler(registryClient)

	// Initial route update
	if err := serviceMesh.UpdateRoutes(); err != nil {
		log.Fatalf("Failed to initialize service mesh routes: %v", err)
	}

	// Periodically update routes from the registry
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			if err := serviceMesh.UpdateRoutes(); err != nil {
				log.Printf("Failed to update routes: %v", err)
			} else {
				log.Println("Service mesh routes updated successfully")
			}
		}
	}()

	// Set up HTTP server with middleware
	mux := http.NewServeMux()
	mux.Handle("/", serviceMesh)

	// Apply middleware: Authentication and Logging
	handler := loggingMiddleware(authMiddleware(mux, apiKey))

	server := &http.Server{
		Addr:    ":8080",
		Handler: handler,
	}

	log.Printf("Cdaprod Service Mesh is running on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}