package domain

import "testing"

func intPtr(i int) *int { return &i }

func TestValidate_Valid(t *testing.T) {
	cases := map[string]CreateServiceRequest{
		"minimal":           {Name: "payments", Language: "go"},
		"with replicas":     {Name: "orders", Language: "go", Replicas: intPtr(3)},
		"with postgres":     {Name: "billing", Language: "go", Database: &Database{Enabled: true, Type: "postgres"}},
		"database disabled": {Name: "gateway", Language: "go", Database: &Database{Enabled: false}},
		"single char name":  {Name: "a", Language: "go"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			svc, errs := req.Validate()
			if errs != nil {
				t.Fatalf("expected valid, got errors: %+v", errs)
			}
			if svc.Status != StatusRegistered {
				t.Errorf("status = %q, want %q", svc.Status, StatusRegistered)
			}
		})
	}
}

func TestValidate_DefaultsReplicas(t *testing.T) {
	svc, errs := CreateServiceRequest{Name: "svc", Language: "go"}.Validate()
	if errs != nil {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if svc.Replicas != DefaultReplicas {
		t.Errorf("replicas = %d, want default %d", svc.Replicas, DefaultReplicas)
	}
}

func TestValidate_Env(t *testing.T) {
	// valid env
	svc, errs := CreateServiceRequest{
		Name: "svc", Language: "go",
		Env: []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}, {Name: "PORT", Value: "8080"}},
	}.Validate()
	if errs != nil {
		t.Fatalf("expected valid env, got %+v", errs)
	}
	if len(svc.Env) != 2 {
		t.Errorf("env not carried over: %+v", svc.Env)
	}

	// invalid name and duplicate
	cases := map[string][]EnvVar{
		"empty name":   {{Name: "", Value: "x"}},
		"invalid name": {{Name: "log-level", Value: "x"}},
		"lowercase ok": {{Name: "goodName", Value: "x"}}, // actually valid; kept out below
		"duplicate":    {{Name: "A", Value: "1"}, {Name: "A", Value: "2"}},
	}
	for name, env := range cases {
		if name == "lowercase ok" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, errs := CreateServiceRequest{Name: "svc", Language: "go", Env: env}.Validate()
			if errs == nil {
				t.Errorf("expected env validation error for %s", name)
			}
		})
	}
}

func TestValidate_Invalid(t *testing.T) {
	cases := map[string]struct {
		req   CreateServiceRequest
		field string
	}{
		"missing name":        {CreateServiceRequest{Language: "go"}, "name"},
		"invalid name upper":  {CreateServiceRequest{Name: "Payments", Language: "go"}, "name"},
		"invalid name symbol": {CreateServiceRequest{Name: "pay_ments", Language: "go"}, "name"},
		"missing language":    {CreateServiceRequest{Name: "svc"}, "language"},
		"language not go":     {CreateServiceRequest{Name: "svc", Language: "python"}, "language"},
		"replicas too low":    {CreateServiceRequest{Name: "svc", Language: "go", Replicas: intPtr(0)}, "replicas"},
		"replicas too high":   {CreateServiceRequest{Name: "svc", Language: "go", Replicas: intPtr(11)}, "replicas"},
		"db enabled no type":  {CreateServiceRequest{Name: "svc", Language: "go", Database: &Database{Enabled: true}}, "database.type"},
		"db unsupported type": {CreateServiceRequest{Name: "svc", Language: "go", Database: &Database{Enabled: true, Type: "mysql"}}, "database.type"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, errs := tc.req.Validate()
			if errs == nil {
				t.Fatalf("expected errors for %s, got none", name)
			}
			found := false
			for _, e := range errs {
				if e.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("expected an error on field %q, got %+v", tc.field, errs)
			}
		})
	}
}
