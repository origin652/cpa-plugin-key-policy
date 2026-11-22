package plugin

import (
	"encoding/json"
	"net/http"
	"testing"
)

func callModelListInterceptor(t *testing.T, app *App, headers http.Header, body string) ResponseInterceptResponse {
	t.Helper()
	req, _ := json.Marshal(ResponseInterceptRequest{
		Kind:           responseKindModelList,
		SourceFormat:   "openai",
		RequestHeaders: headers,
		Body:           []byte(body),
	})
	raw, err := app.HandleMethod(MethodResponseInterceptAfter, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp ResponseInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAppModelListFilteredForPluginKey(t *testing.T) {
	app, plain := configureTestApp(t)
	resp := callModelListInterceptor(t, app, http.Header{"Authorization": {"Bearer " + plain}},
		`{"object":"list","data":[{"id":"fast"},{"id":"gpt-5-codex"},{"id":"other"}]}`)
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("body %s: %v", resp.Body, err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "fast" {
		t.Fatalf("filtered body = %s", resp.Body)
	}
}

func TestAppModelListUntouchedForForeignKey(t *testing.T) {
	app, _ := configureTestApp(t)
	resp := callModelListInterceptor(t, app, http.Header{"Authorization": {"Bearer builtin-cpa-key"}},
		`{"object":"list","data":[{"id":"fast"},{"id":"other"}]}`)
	if len(resp.Body) != 0 {
		t.Fatalf("foreign key list must be untouched, got %s", resp.Body)
	}
}

func TestAppExecutionResponseNotTreatedAsModelList(t *testing.T) {
	app, plain := configureTestApp(t)
	req, _ := json.Marshal(ResponseInterceptRequest{
		Kind:           "execution",
		RequestedModel: "fast",
		RequestHeaders: http.Header{"Authorization": {"Bearer " + plain}},
		Body:           []byte(`{"id":"resp","model":"gpt-5-codex","data":[{"id":"other"}]}`),
	})
	raw, err := app.HandleMethod(MethodResponseInterceptAfter, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	_ = json.Unmarshal(raw, &env)
	var resp ResponseInterceptResponse
	_ = json.Unmarshal(env.Result, &resp)
	var out struct {
		Model string `json:"model"`
		Data  []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("execution body %s: %v", resp.Body, err)
	}
	if out.Model != "fast" || len(out.Data) != 1 || out.Data[0].ID != "other" {
		t.Fatalf("execution body = %s", resp.Body)
	}
}
