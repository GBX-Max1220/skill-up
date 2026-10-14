package evaluator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/skill-up/internal/agent"
	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/credential"
	"github.com/alibaba/skill-up/internal/judge"
	"github.com/alibaba/skill-up/internal/runtime"
	"github.com/alibaba/skill-up/pkg/transcript"
)

func TestIndependentJudgeEngineLifecycle(t *testing.T) {
	for _, test := range []struct {
		name, runtimeType    string
		installErr, checkErr error
		wantInstall          int32
		wantError            string
	}{
		{name: "host", runtimeType: "none"},
		{name: "docker", runtimeType: "docker", wantInstall: 1},
		{name: "opensandbox", runtimeType: "opensandbox", wantInstall: 1},
		{name: "installation failure", runtimeType: "docker", wantInstall: 1, installErr: errors.New("install failed"), wantError: "failed to install judge agent"},
		{name: "missing host CLI", runtimeType: "none", checkErr: errors.New("missing CLI"), wantError: "preflight failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			judgeAgent := &mockAgent{name: "claude_code", installErr: test.installErr, checkErr: test.checkErr}
			original := agentDetectWithResolvedConfig
			agentDetectWithResolvedConfig = func(params credential.ResolvedAgentConfig) (agent.Agent, error) {
				if params.Engine != "claude-code" || params.AppliedVersion != "1.2.3" || params.AppliedModel != "judge-model" || len(params.Kwargs) != 0 {
					t.Fatalf("judge config=%+v", params)
				}
				return judgeAgent, nil
			}
			t.Cleanup(func() { agentDetectWithResolvedConfig = original })
			e := newTestEvaluator(EvalOptions{
				EvalCfg:      &config.EvalConfig{Environment: config.Environment{Type: test.runtimeType}},
				RunnerConfig: credential.ResolvedAgentConfig{Engine: "codex", Version: "9.9.9", Kwargs: map[string]string{"bypass_sandbox": "true"}},
			})
			cfg := config.JudgeConfig{Type: "agent_judge", Engine: &config.EngineConfig{
				Name: "claude-code", Version: "1.2.3", Model: config.ModelConfig{Provider: "anthropic", Name: "judge-model"},
			}, Criteria: []string{"quality"}}
			created, err := e.newJudgeForCase(context.Background(), &mockRuntime{workspace: t.TempDir()}, "without_skill", cfg, &mockAgent{name: "codex"})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v, want %q", err, test.wantError)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if semantic, ok := created.(*judge.AgentJudge); !ok || semantic.Model != "anthropic/judge-model" {
					t.Fatal("judge model metadata missing")
				}
			}
			if judgeAgent.installCall.Load() != test.wantInstall {
				t.Fatalf("install calls=%d", judgeAgent.installCall.Load())
			}
			wantCheck := int32(1)
			if test.installErr != nil {
				wantCheck = 0
			}
			if judgeAgent.checkCall.Load() != wantCheck {
				t.Fatal("incorrect judge preflight calls")
			}
		})
	}
}

func TestIncompatibleJudgeFailsBeforeRunnerExecution(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "multi"}[multi], func(t *testing.T) {
			ag := &mockAgent{name: "codex"}
			member := config.JudgeConfig{ID: "semantic", Type: "agent_judge", Model: "anthropic/claude-model", Criteria: []string{"quality"}}
			cfg := &config.EvalConfig{Environment: config.Environment{Type: "none"}, Engine: config.EngineConfig{Name: "codex"}, Judge: member}
			if multi {
				cfg.Judge = config.JudgeConfig{}
				cfg.Judges = &[]config.JudgeConfig{member}
			}
			e := newTestEvaluator(EvalOptions{Agent: ag, EvalCfg: cfg, RunnerConfig: credential.ResolvedAgentConfig{Engine: "codex", Provider: "openai"}})
			var result EvalResult
			if multi {
				rt, err := runtime.NewRuntime(runtime.Config{Type: "none", WorkspaceDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				if err := rt.Create(context.Background()); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = rt.Close() })
				result = e.executeCase(context.Background(), &config.CaseConfig{ID: "case", Input: config.Input{Prompt: "task"}}, "without_skill", rt, ag)
			} else {
				result = e.executeCase(context.Background(), &config.CaseConfig{ID: "case", Input: config.Input{Prompt: "task"}}, "without_skill", &mockRuntime{workspace: t.TempDir()}, ag)
			}
			if result.Status != judge.StatusError || result.Error == nil || !strings.Contains(result.Error.Error(), "incompatible") {
				t.Fatalf("result=%+v", result)
			}
			if ag.runCall.Load() != 0 {
				t.Fatal("runner executed with incompatible judge")
			}
			if multi && (len(result.JudgeResults) != 1 || result.JudgeResults[0].SkipReason != "invalid_judge_config") {
				t.Fatalf("outcomes=%+v", result.JudgeResults)
			}
		})
	}
}

func TestIndependentJudgesUseSeparateAgentsAndWorkspaceForks(t *testing.T) {
	workspace := t.TempDir()
	rt, err := runtime.NewRuntime(runtime.Config{Type: "none", WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	runAgent := &mockAgent{name: "runner", runFunc: func(_ context.Context, current runtime.Runtime, _ agent.ExecOptions, _ []transcript.Message) (*agent.SessionResult, error) {
		if err := os.WriteFile(filepath.Join(current.Workspace(), "answer.txt"), []byte("completed"), 0o600); err != nil {
			return nil, err
		}
		return &agent.SessionResult{FinalMessage: "completed"}, nil
	}}
	members := []config.JudgeConfig{
		{ID: "quality", Type: "agent_judge", Engine: &config.EngineConfig{Name: "claude_code", Model: config.ModelConfig{Provider: "anthropic", Name: "review-model"}}, Criteria: []string{"quality"}},
		{ID: "style", Type: "agent_judge", Engine: &config.EngineConfig{Name: "codex", Model: config.ModelConfig{Provider: "openai", Name: "review-model"}}, Criteria: []string{"style"}},
	}
	judges := map[string]*mockAgent{}
	original := agentDetectWithResolvedConfig
	agentDetectWithResolvedConfig = func(params credential.ResolvedAgentConfig) (agent.Agent, error) {
		ag := &mockAgent{name: params.Engine, runFunc: independentJudgeWorkspaceCheck(t, workspace, params.Engine)}
		judges[params.Engine] = ag
		return ag, nil
	}
	t.Cleanup(func() { agentDetectWithResolvedConfig = original })
	e := newTestEvaluator(EvalOptions{Agent: runAgent, EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}, Judges: &members}})
	result := e.executeCase(context.Background(), &config.CaseConfig{ID: "case", Input: config.Input{Prompt: "task"}}, "without_skill", rt, runAgent)
	if result.Status != judge.StatusPass || len(result.JudgeResults) != 2 {
		t.Fatalf("result=%+v", result)
	}
	if runAgent.runCall.Load() != 1 || len(judges) != 2 {
		t.Fatal("expected one runner execution and two independent judge agents")
	}
	for name, ag := range judges {
		if ag.runCall.Load() != 1 || ag.installCall.Load() != 0 || ag.checkCall.Load() != 1 {
			t.Fatalf("judge lifecycle incorrect: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "judge-change")); !os.IsNotExist(err) {
		t.Fatal("original workspace modified")
	}
}

func independentJudgeWorkspaceCheck(t *testing.T, workspace, engine string) func(context.Context, runtime.Runtime, agent.ExecOptions, []transcript.Message) (*agent.SessionResult, error) {
	t.Helper()
	return func(_ context.Context, current runtime.Runtime, _ agent.ExecOptions, _ []transcript.Message) (*agent.SessionResult, error) {
		if current.Workspace() == workspace {
			t.Fatal("judge used original workspace")
		}
		data, readErr := os.ReadFile(filepath.Join(current.Workspace(), "answer.txt"))
		if readErr != nil || string(data) != "completed" {
			t.Fatalf("judge input=%q, %v", data, readErr)
		}
		if _, statErr := os.Stat(filepath.Join(current.Workspace(), "judge-change")); !os.IsNotExist(statErr) {
			t.Fatal("sibling mutation leaked")
		}
		if err := os.WriteFile(filepath.Join(current.Workspace(), "judge-change"), []byte(engine), 0o600); err != nil {
			return nil, err
		}
		return &agent.SessionResult{FinalMessage: `{"results":[{"criterion_id":"criterion-1","passed":true,"evidence":["checked"],"failures":[]}]}`}, nil
	}
}

func TestIndependentOpenCodeJudgeProviderConfiguration(t *testing.T) {
	for _, provider := range []string{"openai", "review-provider"} {
		for _, runner := range []string{"codex", "opencode"} {
			t.Run(provider+"/"+runner, func(t *testing.T) {
				ctx := context.Background()
				rt, err := runtime.NewRuntime(runtime.Config{Type: "none", WorkspaceDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				if err := rt.Create(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = rt.Close() })
				rt.MergeEnv(map[string]string{"OPENCODE_CONFIG_CONTENT": `{"mcp":{"existing":{"type":"remote","url":"https://mcp.example"}},"provider":{"openai":{"options":{"baseURL":"https://runner.example/v1"}},"review-provider":{"options":{"baseURL":"https://runner.example/v1"}}}}`})
				judgeAgent := agent.NewOpenCodeAgent(agent.Config{Name: "opencode", CheckCmd: "true", VersionCmd: "printf test", ModelProvider: provider, ModelName: "judge-model", BaseURL: "https://judge.example/v1", APIKey: "judge-test-key"})
				original := agentDetectWithResolvedConfig
				agentDetectWithResolvedConfig = func(credential.ResolvedAgentConfig) (agent.Agent, error) { return judgeAgent, nil }
				t.Cleanup(func() { agentDetectWithResolvedConfig = original })
				e := newTestEvaluator(EvalOptions{EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}}})
				_, err = e.newJudgeForCase(ctx, rt, "without_skill", config.JudgeConfig{Type: "agent_judge", Engine: &config.EngineConfig{Name: "opencode", Model: config.ModelConfig{Provider: provider, Name: "judge-model", BaseURL: "https://judge.example/v1"}}, Criteria: []string{"quality"}}, &mockAgent{name: runner})
				if err != nil {
					t.Fatal(err)
				}
				result, err := rt.Exec(ctx, `printf '%s' "$OPENCODE_CONFIG_CONTENT"`, runtime.ExecOptions{})
				if err != nil {
					t.Fatal(err)
				}
				assertOpenCodeJudgeConfiguration(ctx, t, rt, provider, result.Stdout)
			})
		}
	}
}

func TestIndependentJudgeRuntimeConfigurationFailure(t *testing.T) {
	ctx := context.Background()
	rt, err := runtime.NewRuntime(runtime.Config{Type: "none", WorkspaceDir: t.TempDir(), Env: map[string]string{"OPENCODE_CONFIG_CONTENT": "invalid-json"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Create(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	original := agentDetectWithResolvedConfig
	agentDetectWithResolvedConfig = func(credential.ResolvedAgentConfig) (agent.Agent, error) {
		return agent.NewOpenCodeAgent(agent.Config{Name: "opencode", CheckCmd: "true", VersionCmd: "printf test", BaseURL: "https://judge.example/v1"}), nil
	}
	t.Cleanup(func() { agentDetectWithResolvedConfig = original })
	e := newTestEvaluator(EvalOptions{EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}}})
	_, err = e.newJudgeForCase(ctx, rt, "without_skill", config.JudgeConfig{Type: "agent_judge", Engine: &config.EngineConfig{Name: "opencode", Model: config.ModelConfig{Provider: "openai", Name: "judge-model"}}, Criteria: []string{"quality"}}, &mockAgent{name: "runner"})
	if err == nil || !strings.Contains(err.Error(), "failed to initialize judge agent") {
		t.Fatalf("error=%v", err)
	}
}

func assertOpenCodeJudgeConfiguration(ctx context.Context, t *testing.T, rt runtime.Runtime, provider, content string) {
	t.Helper()
	var value struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
		MCP map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		t.Fatal(err)
	}
	entry := value.Provider[provider]
	if entry.Options.BaseURL != "https://judge.example/v1" || value.MCP["existing"] == nil {
		t.Fatalf("judge endpoint or MCP configuration lost: %s", content)
	}
	if provider == "review-provider" {
		if entry.Options.APIKey != "{env:SKILL_UP_OPENCODE_API_KEY}" || entry.Models["judge-model"] == nil {
			t.Fatalf("custom provider binding missing: %s", content)
		}
		key, err := rt.Exec(ctx, `test "$SKILL_UP_OPENCODE_API_KEY" = judge-test-key`, runtime.ExecOptions{})
		if err != nil || key.ExitCode != 0 {
			t.Fatalf("judge API key binding missing: %v", err)
		}
	}
	if strings.Contains(content, "judge-test-key") {
		t.Fatal("API key embedded in config")
	}
}
