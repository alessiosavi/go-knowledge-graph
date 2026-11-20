package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	AuthStatic = "static"
	AuthGCP    = "gcp"
)

type GCPConfig struct {
	Credentials string         `yaml:"credentials"`
	Route       GCPRouteConfig `yaml:"route"`
}

type GCPRouteConfig struct {
	Host     string `yaml:"host"`
	Project  string `yaml:"project"`
	Location string `yaml:"location"`
}

var (
	routeHost = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*$`)
	routePart = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

func (c Config) validateLLM() error {
	if c.GCP.Credentials != "metadata" {
		return errors.New("config: gcp.credentials must be metadata")
	}
	for _, role := range []struct {
		name, auth, baseURL, key string
		temperature              *float64
		maxTokens                *int
	}{
		{"judge", c.Judge.Auth, c.Judge.BaseURL, c.Judge.APIKey, c.Judge.Temperature, c.Judge.MaxTokens},
		{"agent", c.Agent.Auth, c.Agent.BaseURL, c.Agent.APIKey, c.Agent.Temperature, c.Agent.MaxTokens},
	} {
		if role.temperature != nil && !(0 <= *role.temperature && *role.temperature <= 2) {
			return fmt.Errorf("config: %s.temperature must be finite and between 0 and 2", role.name)
		}
		if role.maxTokens != nil && *role.maxTokens < 1 {
			return fmt.Errorf("config: %s.max_tokens must be at least 1", role.name)
		}
		switch role.auth {
		case AuthStatic:
			continue
		case AuthGCP:
		default:
			return fmt.Errorf("config: %s.auth must be static or gcp", role.name)
		}
		if role.key != "" {
			return fmt.Errorf("config: %s.api_key must be empty with auth=gcp", role.name)
		}
		route := c.GCP.Route
		if !routeHost.MatchString(route.Host) || !routePart.MatchString(route.Project) || !routePart.MatchString(route.Location) {
			return errors.New("config: gcp.route requires a host, project and location without URL delimiters")
		}
		want := "https://" + route.Host + "/v1/projects/" + route.Project + "/locations/" + route.Location + "/endpoints/openapi"
		if strings.TrimRight(role.baseURL, "/") != want {
			return fmt.Errorf("config: %s.base_url must match the HTTPS gcp.route", role.name)
		}
	}
	return nil
}
