package marathon

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gambol99/go-marathon"
	"github.com/hashicorp/consul/api"
	"github.com/tionebsalocin/consul-wrapper/runner"
)

const (
	consulAppNameLabel = "consul_service_name"

	dateFormat = "20060102T150405Z07:00"
)

type Config struct {
	AppID         string
	MarathonURL   string
	PortOverrides map[int]int
}

func New(cfg Config) (runner.Config, error) {
	appConfig := runner.Config{}

	client, err := marathon.NewClient(marathon.Config{
		URL: cfg.MarathonURL,
	})
	if err != nil {
		return appConfig, err
	}

	app, err := client.Application(cfg.AppID)
	if err != nil {
		return appConfig, err
	}

	app = applyPortsOverride(cfg, app)

	consulRegistrations, err := makeConsulApp(app)
	if err != nil {
		return appConfig, err
	}

	appConfig.Definition = consulRegistrations

	return appConfig, nil
}

func makeConsulApp(app *marathon.Application) ([]*api.AgentServiceRegistration, error) {

	appName := defaultAppName(app)
	globalTags := globalTags(app)

	checks, portTags := makeHealthChecks(app)

	hasMainService := false

	res := []*api.AgentServiceRegistration{}

	for _, portDef := range *app.PortDefinitions {
		if portDef.Port == nil || *portDef.Port <= 0 {
			continue
		}

		if hasMainService && portDef.Name == "" {
			continue
		}

		if portDef.Labels != nil && (*portDef.Labels)["consul_registration"] == "no" || (*portDef.Labels)["consul_registration"] == "false" {
			continue
		}

		tags := []string{}
		tags = append(tags, globalTags...)
		if portDef.Labels != nil && (*portDef.Labels)["ctags"] != "" {
			tags = append(tags, strings.Split((*portDef.Labels)["ctags"], ",")...)
		}
		if app.Labels != nil && (*app.Labels)["ctags"] != "" {
			tags = append(tags, strings.Split((*app.Labels)["ctags"], ",")...)
		}

		s := makeConsulService(app, portDef, tags)
		res = append(res, s)
	}

	return res, nil
}

func globalTags(app *marathon.Application) []string {
	globalTags := []string{
		fmt.Sprintf("marathon-start-%s", time.Now().Format(dateFormat)),
	}

	// TODO mesos app id

	if app.User != "" {
		globalTags = append(globalTags, fmt.Sprintf("marathon-user-%s", app.User))
	}
	if app.RequirePorts != nil && *app.RequirePorts {
		globalTags = append(globalTags, "marathon-requirePort")
	}

	return globalTags
}

func defaultAppName(app *marathon.Application) string {
	appName := app.ID

	if app.Labels == nil {
		return appName
	}
	if name, ok := (*app.Labels)[consulAppNameLabel]; ok {
		appName = name
	}

	return appName
}

func applyPortsOverride(cfg Config, app *marathon.Application) *marathon.Application {
	if app.PortDefinitions == nil {
		return app
	}
	for i, port := range *app.PortDefinitions {
		override, ok := cfg.PortOverrides[i]
		if !ok {
			continue
		}

		port.Port = &override
	}

	return app
}

func makeHealthChecks(app *marathon.Application) ([]*api.AgentCheckRegistration, map[int][]string) {
	if app.HealthChecks == nil {
		return nil, nil
	}

	getPort := func(hc marathon.HealthCheck) int {
		if hc.Port != nil && *hc.Port > 0 {
			return *hc.Port
		}

		if hc.PortIndex != nil {
			if app.PortDefinitions == nil {
				return 0
			}
			if len(*app.PortDefinitions)-1 < *hc.PortIndex {
				return 0
			}
			pd := (*app.PortDefinitions)[*hc.PortIndex]
			if pd.Port == nil {
				return 0
			}
			return *pd.Port
		}

		return 0
	}

	checks := []*api.AgentCheckRegistration{}
	tags := map[int][]string{}

	for i, hc := range *app.HealthChecks {
		timeout := time.Duration(hc.TimeoutSeconds) * time.Second
		interval := time.Duration(hc.IntervalSeconds) * time.Second

		switch hc.Protocol {
		case "HTTP", "HTTPS", "MESOS_HTTP", "MESOS_HTTPS":
			port := getPort(hc)
			if port <= 0 {
				log.Printf("[consulWrapper] Bad port definition for marathon healthcheck %+v", hc)
				continue
			}

			proto := "http"
			if strings.Contains(hc.Protocol, "HTTPS") {
				proto = "https"
			}

			path := "/"
			if hc.Path != nil {
				path = *hc.Path
			}

			checks = append(checks, &api.AgentCheckRegistration{
				Name: fmt.Sprintf("marathon_http_check_%d", i),
				AgentServiceCheck: api.AgentServiceCheck{
					HTTP:     fmt.Sprintf("%s://localhost:%d%s", proto, port, path),
					Notes:    fmt.Sprintf("%s Marathon HealthCheck: %+v", proto, hc),
					Timeout:  timeout.String(),
					Interval: (time.Duration(hc.IntervalSeconds) * time.Second).String(),
				},
			})
			tags[port] = append(tags[port], "http")

		case "TCP", "MESOS_TCP":
			port := getPort(hc)
			if port <= 0 {
				log.Printf("[consulWrapper] Bad port definition for marathon healthcheck %+v", hc)
				continue
			}

			checks = append(checks, &api.AgentCheckRegistration{
				Name: fmt.Sprintf("marathon_tcp_check_%d", i),
				AgentServiceCheck: api.AgentServiceCheck{
					TCP:      fmt.Sprintf("localhost:%d", port),
					Notes:    fmt.Sprintf("tpc Marathon HealthCheck: %+v", hc),
					Timeout:  timeout.String(),
					Interval: interval.String(),
				},
			})
			tags[port] = append(tags[port], "http")

		default:
			log.Printf("[consulWrapper] Ignoring health check of type %s", hc.Protocol)
		}
	}

	return checks, tags
}

func appName(defaultAppName string, portDef *marathon.PortDefinition) string {
	clean := func(s string) string {
		s = strings.ReplaceAll(s, "/", "-")
		s = strings.ReplaceAll(s, ".", "-")
		if s[0] == '-' {
			s = s[1:]
		}
		return s
	}

	if portDef.Labels != nil && (*portDef.Labels)["consul_service_name"] != "" {
		return clean((*portDef.Labels)["consul_service_name"])
	}

	if portDef.Name != "" {
		return clean(fmt.Sprintf("%s-%s", defaultAppName, portDef.Name))
	}

	return clean(defaultAppName)
}

func formatMeta(k, v string) (string, string, bool) {
	if len(k) > 128 {
		return "", "", false
	}
	if len(v) > 512 {
		return "", "", false
	}
	if strings.HasPrefix(strings.ToLower(k), "consul_") {
		return "", "", false
	}
	if strings.HasPrefix(k, "DNS_ENTRY") {
		return "", "", false
	}
	if strings.ToLower(k) == "deregister_critical_service_after" {
		return "", "", false
	}

	if k == "weight" {
		k = "original_weight"
	}
	k = strings.ReplaceAll(k, ".", "_")

	return k, v, true
}

func serviceMeta(app *marathon.Application, portDef *marathon.PortDefinition) map[string]string {
	meta := map[string]string{
		"marathon_app_version": app.Version,
		"start":                time.Now().Format(dateFormat),
	}

	if app.Labels != nil {
		for k, v := range *app.Labels {
			if k, v, ok := formatMeta(k, v); ok {
				meta[k] = v
			}

		}
	}
	if portDef.Labels != nil {
		for k, v := range *portDef.Labels {
			if k, v, ok := formatMeta(k, v); ok {
				meta[k] = v
			}

		}
	}
	return meta
}

func serviceWeights(app *marathon.Application, portDef *marathon.PortDefinition) *api.AgentWeights {
	w := &api.AgentWeights{
		Passing: 10,
	}

	if app.Labels != nil {
		for k, v := range *app.Labels {
			if k != "weight" {
				continue
			}
			val, err := strconv.Atoi(v)
			if err != nil {
				log.Printf("[consulWrapper] Error parsing weight '%s': %s", v, err)
				break
			}
			w.Passing = val
		}
	}
	if portDef.Labels != nil {
		for k, v := range *portDef.Labels {
			if k != "weight" {
				continue
			}
			val, err := strconv.Atoi(v)
			if err != nil {
				log.Printf("[consulWrapper] Error parsing weight '%s': %s", v, err)
				break
			}
			w.Passing = val
		}
	}
	w.Warning = w.Passing/10 + 1

	return w
}

func makeConsulService(defaultAppName string, app *marathon.Application, portDef *marathon.PortDefinition, tags []string) *api.AgentServiceRegistration {
	name := appName(defaultAppName, portDef)
	nameEsc := replaceNonAlphaNum(name, '-')
	mesosID := replaceNonAlphaNum(strings.Split(app.ID, ".")[1], -1)

	sort.Strings(tags)

	srv := &api.AgentServiceRegistration{
		ID:      fmt.Sprintf("marathon-app-%s-%d-%d", nameEsc, *portDef.Port, mesosID),
		Name:    name,
		Tags:    tags,
		Meta:    serviceMeta(app, portDef),
		Weights: serviceWeights(app, portDef),
	}

	return srv
}

func replaceNonAlphaNum(s string, with rune) string {
	return strings.Map(func(r rune) rune {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return with
		}
		return r
	}, s)
}
