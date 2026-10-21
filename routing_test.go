package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func configureRoutingTest(t *testing.T) {
	t.Helper()
	cfg := defaultConfig()
	dynamic := false
	cfg.DynamicModels = &dynamic
	cfg.Models = []string{"zcode/glm-5.3", "zcode/glm-5.3-flash"}
	cfg.ModelMap = map[string]string{"zcode/glm-5.3": "glm-5.3", "zcode/glm-5.3-flash": "glm-5.3-flash"}
	cfg.APIKey = "test-upstream-key"
	cfg.TimeoutSeconds = 30
	activeConfig.Store(cfg)
	t.Cleanup(func() { activeConfig.Store(defaultConfig()) })
}

func TestStaticExecutorDeclaresModelRouter(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Capabilities["model_router"] != true {
		t.Fatal("static models have no model router; requests fall into CPA credential selection")
	}
}

func TestConfiguredModelsRouteToSelf(t *testing.T) {
	configureRoutingTest(t)
	for _, model := range []string{"zcode/glm-5.3", "zcode/glm-5.3-flash", "unrelated-model", "glm-5.2"} {
		t.Run(model, func(t *testing.T) {
			payload, _ := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: model})
			raw, err := handleMethod(pluginabi.MethodModelRoute, payload)
			if err != nil {
				t.Fatal(err)
			}
			var env envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			if !env.OK {
				t.Fatalf("route unsupported: %#v", env.Error)
			}
			var route pluginapi.ModelRouteResponse
			if err := json.Unmarshal(env.Result, &route); err != nil {
				t.Fatal(err)
			}
			want := model == "zcode/glm-5.3" || model == "zcode/glm-5.3-flash"
			if route.Handled != want {
				t.Fatalf("handled=%v, want %v", route.Handled, want)
			}
			if want && route.TargetKind != pluginapi.ModelRouteTargetSelf {
				t.Fatalf("not routed to own executor: %#v", route)
			}
		})
	}
}
