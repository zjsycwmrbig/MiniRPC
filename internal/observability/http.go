package observability

import (
	"bytes"
	"net/http"
)

// PrometheusHandler exposes one runtime's metrics in Prometheus text format.
func PrometheusHandler(runtime *Runtime) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var output bytes.Buffer
		if err := runtime.Metrics().WritePrometheus(&output); err != nil {
			http.Error(writer, "render metrics", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			_, _ = writer.Write(output.Bytes())
		}
	})
}
