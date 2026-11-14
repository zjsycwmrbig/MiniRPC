package observability

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"time"

	otlptracehttp "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/net/http/httpguts"
)

const defaultOTLPExportTimeout = 5 * time.Second

// OTLPExporterConfig controls the standard OTLP/HTTP trace exporter.
type OTLPExporterConfig struct {
	Endpoint      string
	Insecure      bool
	Headers       map[string]string
	Timeout       time.Duration
	TLSConfig     *tls.Config
	TLSMinVersion uint16
}

// NewOTLPTraceExporter validates an OTLP/HTTP endpoint and creates a trace
// exporter. Plain HTTP endpoints require Insecure so transport security is an
// explicit operator decision.
func NewOTLPTraceExporter(
	ctx context.Context,
	config OTLPExporterConfig,
) (sdktrace.SpanExporter, error) {
	if ctx == nil {
		return nil, fmt.Errorf("observability: nil OTLP context")
	}
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("observability: OTLP endpoint must be an absolute URL")
	}
	if endpoint.User != nil || endpoint.Fragment != "" {
		return nil, fmt.Errorf("observability: OTLP endpoint cannot contain credentials or a fragment")
	}
	switch endpoint.Scheme {
	case "http":
		if !config.Insecure {
			return nil, fmt.Errorf("observability: plain HTTP OTLP endpoint requires explicit insecure mode")
		}
		if config.TLSConfig != nil {
			return nil, fmt.Errorf("observability: OTLP TLS configuration requires an HTTPS endpoint")
		}
	case "https":
		if config.Insecure {
			return nil, fmt.Errorf("observability: insecure mode cannot be used with an HTTPS OTLP endpoint")
		}
		if config.TLSMinVersion != 0 && config.TLSMinVersion < tls.VersionTLS12 {
			return nil, fmt.Errorf("observability: OTLP TLS minimum version must be 1.2 or newer")
		}
		if config.TLSConfig != nil && config.TLSConfig.InsecureSkipVerify {
			return nil, fmt.Errorf("observability: OTLP TLS configuration cannot skip verification")
		}
		if config.TLSConfig != nil &&
			config.TLSConfig.MinVersion != 0 &&
			config.TLSConfig.MinVersion < tls.VersionTLS12 {
			return nil, fmt.Errorf("observability: OTLP TLS minimum version must be 1.2 or newer")
		}
	default:
		return nil, fmt.Errorf("observability: OTLP endpoint scheme must be http or https")
	}
	for key, value := range config.Headers {
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("observability: invalid OTLP header %q", key)
		}
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultOTLPExportTimeout
	}
	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(endpoint.String()),
		otlptracehttp.WithTimeout(timeout),
	}
	if endpoint.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	if endpoint.Scheme == "https" {
		var tlsConfig *tls.Config
		if config.TLSConfig == nil {
			tlsConfig = &tls.Config{}
		} else {
			tlsConfig = config.TLSConfig.Clone()
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = config.TLSMinVersion
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = endpoint.Hostname()
		}
		options = append(options, otlptracehttp.WithTLSClientConfig(tlsConfig))
	}
	if len(config.Headers) > 0 {
		options = append(options, otlptracehttp.WithHeaders(config.Headers))
	}
	return otlptracehttp.New(ctx, options...)
}

// ParseOTLPHeaders parses a comma-separated list of static exporter headers.
// Values are returned separately from errors so logs never need to echo
// credentials from an invalid input string.
func ParseOTLPHeaders(value string) (map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	headers := make(map[string]string)
	for _, entry := range strings.Split(value, ",") {
		key, headerValue, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		headerValue = strings.TrimSpace(headerValue)
		if !ok || key == "" {
			return nil, fmt.Errorf("observability: OTLP header must use key=value syntax")
		}
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(headerValue) {
			return nil, fmt.Errorf("observability: invalid OTLP header %q", key)
		}
		if _, exists := headers[key]; exists {
			return nil, fmt.Errorf("observability: duplicate OTLP header %q", key)
		}
		headers[key] = headerValue
	}
	return headers, nil
}
