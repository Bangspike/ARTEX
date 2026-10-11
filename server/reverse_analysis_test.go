package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/mcpstdio"
	"github.com/Autumn-27/artex/reanalysis"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/google/uuid"
)

// These tests only use an explicitly supplied, disposable local database.
func reverseTestServer(t *testing.T) (*Server, *db.Task) {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("set ARTEX_PG_DSN to a disposable local database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || !strings.Contains(u.Path, "test") {
		t.Fatal("reverse tests require an explicitly named local test database")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var port, dataDir, database string
	if err := pg.QueryRow(`SELECT current_setting('port'),current_setting('data_directory'),current_database()`).Scan(&port, &dataDir, &database); err != nil {
		t.Fatal(err)
	}
	t.Logf("local database: %s port=%s data=%s", database, port, dataDir)
	task, err := pg.CreateTask("local reverse fixture", "static analysis", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ctx: ctx, m: &Manager{pg: pg, dir: t.TempDir(), tasks: map[string]*Task{}}, chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	id := strconv.FormatInt(task.ID, 10)
	s.m.tasks[id] = &Task{ID: id, ExpID: task.ExplorationID, Store: pg.Exploration(task.ExplorationID), notify: make(chan struct{}, 1)}
	if err := s.seedReverseAnalyst(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		waitRetestIdle(t, s)
		_, _ = pg.Exec(`DELETE FROM conversations WHERE id IN (SELECT conversation_id FROM reverse_analyses WHERE task_id=$1)`, task.ID)
		_ = pg.DeleteTask(task.ID)
		pg.Close()
	})
	return s, task
}

func reverseTestJob(t *testing.T, s *Server, task *db.Task, kind, path string, digest ...string) *db.ReverseAnalysis {
	t.Helper()
	a := &db.ReverseAnalysis{ID: uuid.NewString(), TaskID: task.ID, Kind: kind, InputPath: path, InputSHA256: strings.Repeat("a", 64), Objective: "inspect fixture", Status: "pending"}
	if len(digest) > 0 {
		a.InputSHA256 = digest[0]
	}
	c, err := s.m.pg.CreateReverseAnalysis(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.m.pg.StartReverseAnalysis(t.Context(), c.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	a.Status = "running"
	return a
}

func TestReverseReportBindingAndLifecycle(t *testing.T) {
	s, task := reverseTestServer(t)
	a := reverseTestJob(t, s, task, "javascript", "fixture.js")
	duplicate := &db.ReverseAnalysis{ID: uuid.NewString(), TaskID: task.ID, Kind: "javascript", InputPath: "fixture.js", Objective: "duplicate"}
	if _, err := s.m.pg.CreateReverseAnalysis(t.Context(), duplicate); !errors.Is(err, db.ErrAnalysisBusy) {
		t.Fatal("duplicate accepted", err)
	}
	ctx := intercept.WithConvID(t.Context(), a.ConversationID)
	complete := workflowTool(t, s.reverseAnalysisTools(), "complete_reverse_analysis")
	c := db.ReverseConclusion{Status: "completed", Summary: "static fixture", Report: "Observed a route; authorization is unverified.", EvidenceIDs: []string{uuid.NewString()}}
	workflowCall(t, ctx, complete, c, true)
	var ids []string
	for _, name := range []string{"analyze_javascript_application", "export_evidence_bundle"} {
		if name == "export_evidence_bundle" {
			if err := os.MkdirAll(filepath.Dir(s.reverseExportPath(a)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.reverseExportPath(a), []byte(`{"evidence":"fixture"}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		receipt, err := s.retainReverseEvidence(ctx, a, name, json.RawMessage(`{}`), json.RawMessage(`{"content":[],"structuredContent":{"evidence_id":"ev_fixture"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var r map[string]string
		_ = json.Unmarshal([]byte(receipt), &r)
		ids = append(ids, r["artex_evidence_id"])
		c.EvidenceIDs = ids
		if name != "export_evidence_bundle" {
			workflowCall(t, ctx, complete, c, true)
		}
	}
	workflowCall(t, context.Background(), complete, c, true)
	if _, err := s.m.pg.Exec(`UPDATE reverse_analysis_evidence SET created_at=now()-interval '1 minute' WHERE analysis_id=$1 AND tool='export_evidence_bundle'`, a.ID); err != nil {
		t.Fatal(err)
	}
	workflowCall(t, ctx, complete, c, true)
	if _, err := s.m.pg.Exec(`UPDATE reverse_analysis_evidence SET created_at=now() WHERE analysis_id=$1 AND tool='export_evidence_bundle'`, a.ID); err != nil {
		t.Fatal(err)
	}
	workflowCall(t, ctx, complete, c, false)
	workflowCall(t, ctx, complete, c, true)
	if err := s.m.pg.FinishReverseAnalysis(a.ConversationID, "failed", "must not overwrite"); err != nil {
		t.Fatal(err)
	}
	saved, err := s.m.pg.GetReverseAnalysis(ctx, task.ID, a.ID)
	if err != nil || saved.Status != "completed" || saved.FactNodeID == 0 {
		t.Fatal(saved, err)
	}
	node, err := s.m.pg.Exploration(task.ExplorationID).GetNode(saved.FactNodeID)
	if err != nil || !strings.Contains(string(node.Payload), ids[0]) {
		t.Fatal("missing task fact evidence", node, err)
	}
	if _, err := s.m.pg.GetReverseAnalysis(ctx, task.ID+1, a.ID); err == nil {
		t.Fatal("cross-task read accepted")
	}
	// Downloads verify the bytes rather than trusting the stored path alone.
	rows, _ := s.m.pg.ReverseEvidence(ctx, a.ID)
	download := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("id", strconv.FormatInt(task.ID, 10))
		r.SetPathValue("analysis", a.ID)
		r.SetPathValue("evidence", rows[0].ID)
		w := httptest.NewRecorder()
		s.reverseEvidenceDownload(w, r)
		return w
	}
	if w := download(); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if err := os.WriteFile(filepath.Join(s.m.dir, rows[0].Path), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := download(); w.Code != 409 {
		t.Fatal("tampered download accepted", w.Code)
	}
	next := reverseTestJob(t, s, task, "native", "fixture.bin")
	workflowCall(t, intercept.WithConvID(t.Context(), next.ConversationID), complete, c, true)
	if err := s.m.pg.RecoverReverseAnalyses(); err != nil {
		t.Fatal(err)
	}
	resumed, _ := s.m.pg.GetReverseAnalysis(t.Context(), task.ID, next.ID)
	if resumed.Status != "stopped" {
		t.Fatal(resumed)
	}
	if ok, err := s.m.pg.StartReverseAnalysis(t.Context(), next.ConversationID); err != nil || ok {
		t.Fatal("sealed run restarted", ok, err)
	}
}

// Uses the actual REA stdio server, including its JS/Electron parser and optional
// Ghidra. No external LLM is involved; deterministic calls isolate the adapter.
func TestReverseRealREA(t *testing.T) {
	command, script := os.Getenv("ARTEX_REA_COMMAND"), os.Getenv("ARTEX_REA_SCRIPT")
	if command == "" || script == "" {
		t.Skip("set ARTEX_REA_COMMAND and ARTEX_REA_SCRIPT for real-engine validation")
	}
	for _, kind := range []string{"javascript", "native"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "native" && (os.Getenv("ARTEX_REA_GHIDRA_DIR") == "" || os.Getenv("ARTEX_REA_JAVA_HOME") == "") {
				t.Skip("set local Ghidra and Java homes")
			}
			s, task := reverseTestServer(t)
			input := filepath.Join(s.m.dir, "fixture")
			if err := os.MkdirAll(input, 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "javascript" {
				files := map[string]string{"package.json": `{"name":"artex-electron-fixture","version":"1.0.0","main":"main.cjs","dependencies":{"electron":"^35.0.0"}}`, "main.cjs": `const { app, BrowserWindow, ipcMain } = require('electron'); const { readFile } = require('node:fs/promises'); ipcMain.handle('read-config', async (_event, path) => readFile(path, 'utf8')); app.whenReady().then(() => { const win = new BrowserWindow({webPreferences:{preload:__dirname + '/preload.cjs',contextIsolation:true}}); win.loadFile('index.html'); });`, "preload.cjs": `const {contextBridge,ipcRenderer}=require('electron'); contextBridge.exposeInMainWorld('desktop',{readConfig:(path)=>ipcRenderer.invoke('read-config',path)});`, "index.html": `<script>window.desktop.readConfig('settings.json');</script>`}
				for name, data := range files {
					if err := os.WriteFile(filepath.Join(input, name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				source := filepath.Join(input, "sample.c")
				if err := os.WriteFile(source, []byte(`#include <stdio.h>
int artex_check(int value) { return value == 4242; }
int main(int argc,char **argv) { puts("ARTEX_NATIVE_FIXTURE"); return artex_check(argc); }
`), 0600); err != nil {
					t.Fatal(err)
				}
				input = filepath.Join(input, "sample")
				if out, err := exec.Command("cc", "-g", "-O0", "-o", input, source).CombinedOutput(); err != nil {
					t.Fatalf("compile fixture: %v %s", err, out)
				}
			}
			rel, err := filepath.Rel(s.m.dir, input)
			if err != nil {
				t.Fatal(err)
			}
			input, digest, err := reanalysis.Snapshot(s.m.dir, rel, filepath.Join(s.m.dir, "snapshots"))
			if err != nil {
				t.Fatal(err)
			}
			a := reverseTestJob(t, s, task, kind, input, digest)
			ctx, cancel := context.WithTimeout(intercept.WithConvID(t.Context(), a.ConversationID), 8*time.Minute)
			defer cancel()
			args, _ := json.Marshal([]string{script, "mcp"})
			env, _ := json.Marshal(map[string]string{"GHIDRA_INSTALL_DIR": os.Getenv("ARTEX_REA_GHIDRA_DIR"), "JAVA_HOME": os.Getenv("ARTEX_REA_JAVA_HOME"), "REA_ANALYSIS_PROVIDER": "ghidra"})
			// Empty Java/Ghidra variables are omitted for JS-only validation.
			if kind == "javascript" {
				env = json.RawMessage(`{}`)
			}
			client, err := s.connectAgentMCP(ctx, &db.MCPServer{Name: "rea", Transport: "stdio", Command: command, Args: args, Env: env}, db.ReverseAnalystKey)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			c := client.(*mcpstdio.Client)
			tools, err := c.Tools(ctx)
			if err != nil || len(tools) < 5 {
				t.Fatal("inventory", len(tools), err)
			}
			call := func(name string, args any) string {
				t.Helper()
				b, _ := json.Marshal(args)
				raw, receipt, err := c.Call(ctx, name, b)
				if err != nil {
					t.Fatal(name, err)
				}
				var r struct {
					IsError bool `json:"isError"`
				}
				_ = json.Unmarshal(raw, &r)
				if r.IsError {
					t.Fatalf("%s: %s", name, raw)
				}
				t.Logf("%s: %d bytes retained; %s", name, len(raw), receipt)
				return string(raw)
			}
			call("binary_session", map[string]any{})
			if _, _, err := c.Call(ctx, "run_script", json.RawMessage(`{}`)); err == nil {
				t.Fatal("unaudited tool accepted")
			}
			if kind == "javascript" {
				bad, _ := json.Marshal(map[string]any{"input_path": "/etc/passwd"})
				if _, _, err := c.Call(ctx, "analyze_javascript_application", bad); err == nil {
					t.Fatal("foreign input accepted")
				}
				raw := call("analyze_javascript_application", map[string]any{"input_path": input, "detail": "full"})
				if !strings.Contains(strings.ToLower(raw), "electron") {
					t.Fatal("missing Electron analysis")
				}
			} else {
				call("open_binary", map[string]any{"path": input, "provider_id": "ida"})
				raw := call("binary_overview", map[string]any{})
				if !strings.Contains(strings.ToLower(raw), "ghidra") {
					t.Fatal("missing Ghidra provider evidence")
				}
				procedures := call("search_procedures", map[string]any{"pattern": "artex_check"})
				symbol := "artex_check"
				if strings.Contains(procedures, "_artex_check") {
					symbol = "_artex_check"
				}
				dossier := call("analyze_function", map[string]any{"procedure": symbol})
				var decompiled struct {
					Structured struct {
						Result struct {
							Pseudocode string `json:"pseudocode"`
						} `json:"normalized_result"`
					} `json:"structuredContent"`
				}
				_ = json.Unmarshal([]byte(dossier), &decompiled)
				code := decompiled.Structured.Result.Pseudocode
				if !strings.Contains(code, "4242") && !strings.Contains(code, "0x1092") {
					t.Fatal("missing native decompilation")
				}
				t.Logf("Ghidra pseudocode: %s", code)
				call("search_strings", map[string]any{"pattern": "ARTEX_NATIVE_FIXTURE"})
			}
			call("export_evidence_bundle", map[string]any{"path": "/should-not-write-here", "overwrite": true})
			bundle, err := os.ReadFile(s.reverseExportPath(a))
			if err != nil || !json.Valid(bundle) {
				t.Fatal("invalid full bundle", err)
			}
			marker := "read-config"
			if kind == "native" {
				marker = "ARTEX_NATIVE_FIXTURE"
			}
			if !strings.Contains(string(bundle), marker) {
				t.Fatal("expected source marker missing from evidence bundle")
			}
			rows, err := s.m.pg.ReverseEvidence(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, e := range rows {
				ids = append(ids, e.ID)
			}
			workflowCall(t, ctx, workflowTool(t, s.reverseAnalysisTools(), "complete_reverse_analysis"), db.ReverseConclusion{Status: "completed", Summary: "Real " + kind + " engine fixture", Report: "Observed fixture marker: `" + marker + "`. Static evidence only.", EvidenceIDs: ids, Limitations: []string{"Deterministic test report; external LLM reasoning not tested."}}, false)
			saved, err := s.m.pg.GetReverseAnalysis(ctx, task.ID, a.ID)
			if err != nil || saved.FactNodeID == 0 || saved.Status != "completed" {
				t.Fatal(saved, err)
			}
			if dir := os.Getenv("ARTEX_REA_TEST_OUTPUT"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				b, _ := json.MarshalIndent(map[string]any{"analysis": saved, "evidence": rows}, "", "  ")
				if err := os.WriteFile(filepath.Join(dir, kind+"-report.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, kind+"-bundle.json"), bundle, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReverseConversationRealREA(t *testing.T) {
	command, script := os.Getenv("ARTEX_REA_COMMAND"), os.Getenv("ARTEX_REA_SCRIPT")
	if command == "" || script == "" {
		t.Skip("real REA runtime not configured")
	}
	s, task := reverseTestServer(t)
	oldAugment, oldResolve, oldPrompt, oldTraffic := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve, agent.PromptOverride, agent.FindingTrafficBindingEnabled = oldAugment, oldResolve, oldPrompt, oldTraffic
	})
	wireAgentAugment(s.m.pg, "../skills", func() ([]actool.CoreTool, map[string][]string) { return s.reverseAnalysisTools(), nil }, s.connectAgentMCP)
	wireTools(s.m.pg, nil)
	agent.PromptOverride = func(string) (string, bool) { return reverseAnalystPrompt, true }
	m, err := s.findMCPByName("rea")
	if err != nil || m == nil {
		t.Fatal(m, err)
	}
	original := *m
	t.Cleanup(func() { _, _ = s.m.pg.SaveMCP(&original) })
	m.Command = command
	m.Args, _ = json.Marshal([]string{script, "mcp"})
	m.Env = json.RawMessage(`{}`)
	m.Enabled = true
	if _, err := s.m.pg.SaveMCP(m); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.m.dir, "app"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"package.json": `{"name":"fixture","main":"index.js"}`, "index.js": `export const route = '/api/login';`} {
		if err := os.WriteFile(filepath.Join(s.m.dir, "app", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	setProvider := func(p retestProvider) {
		s.chatAgent = agent.NewChatAgent(p, "test", s.m.dir, nil, 200000)
		s.chatAgent.SetNonStreaming(func() bool { return true })
	}
	setProvider(retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		n := calls.Add(1)
		if len(req.Messages) > 0 {
			for _, block := range req.Messages[len(req.Messages)-1].Content {
				if block.IsError {
					t.Logf("tool failure at turn %d: %+v", n, block.Content)
				}
			}
		}
		rows, err := s.m.pg.ListReverseAnalyses(ctx, task.ID)
		if err != nil || len(rows) != 1 {
			return llm.Message{}, "", llm.Usage{}, errors.New("missing task binding")
		}
		a := rows[0]
		name := ""
		var args any
		switch n {
		case 1:
			name = "Skill"
			args = map[string]any{"name": "rea-analysis"}
		case 2:
			name = "reverse_analysis_context"
			args = map[string]any{}
		case 3:
			name = "ExecuteExtraTool"
			args = map[string]any{"tool_name": "mcp__rea__analyze_javascript_application", "params": map[string]any{"input_path": a.InputPath, "detail": "summary"}}
		case 4:
			name = "ExecuteExtraTool"
			args = map[string]any{"tool_name": "mcp__rea__export_evidence_bundle", "params": map[string]any{"path": s.reverseExportPath(a)}}
		case 5:
			e, err := s.m.pg.ReverseEvidence(ctx, a.ID)
			if err != nil || len(e) != 2 {
				return llm.Message{}, "", llm.Usage{}, errors.New("REA tool sequence did not retain both results")
			}
			name = "complete_reverse_analysis"
			args = db.ReverseConclusion{Status: "completed", Summary: "Login route observed", Report: "Static fixture exposes /api/login; runtime behavior unverified.", EvidenceIDs: []string{e[0].ID, e[1].ID}}
		default:
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Report saved")}}, "end_turn", llm.Usage{}, nil
		}
		in, _ := json.Marshal(args)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: strconv.Itoa(int(n)), Name: name, Input: in}}}, "tool_use", llm.Usage{}, nil
	}})
	w := retestRequest(s.createReverseAnalysis, "POST", task.ID, `{"kind":"javascript","input_path":"app","objective":"Locate the login route"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	waitRetestIdle(t, s)
	var submitted db.ReverseAnalysis
	_ = json.Unmarshal(w.Body.Bytes(), &submitted)
	a, err := s.m.pg.GetReverseAnalysis(t.Context(), task.ID, submitted.ID)
	if err != nil || a.Status != "completed" || a.FactNodeID == 0 || calls.Load() != 6 {
		t.Fatalf("full conversation failed: %+v calls=%d err=%v", a, calls.Load(), err)
	}
	// A stopped Agent cannot later submit a report; its MCP cleanup is drained.
	entered := make(chan struct{})
	setProvider(retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		close(entered)
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}})
	w = retestRequest(s.createReverseAnalysis, "POST", task.ID, `{"kind":"javascript","input_path":"app","objective":"Stop fixture"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &submitted)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Agent not started")
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("id", strconv.FormatInt(task.ID, 10))
	r.SetPathValue("analysis", submitted.ID)
	w = httptest.NewRecorder()
	s.stopReverseAnalysis(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	waitRetestIdle(t, s)
	a, err = s.m.pg.GetReverseAnalysis(t.Context(), task.ID, submitted.ID)
	if err != nil || a.Status != "stopped" || a.FactNodeID != 0 {
		t.Fatal(a, err)
	}
}

func TestReverseArchiveRoundTrip(t *testing.T) {
	s, task := reverseTestServer(t)
	a := reverseTestJob(t, s, task, "javascript", "snapshot.js")
	receipt, err := s.retainReverseEvidence(t.Context(), a, "analyze_javascript_application", json.RawMessage(`{}`), json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"Unsupported input"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		ID string `json:"artex_evidence_id"`
	}
	_ = json.Unmarshal([]byte(receipt), &r)
	node, err := s.m.pg.CompleteReverseAnalysis(t.Context(), a.ConversationID, db.ReverseConclusion{Status: "partial", Summary: "Incomplete fixture", Report: "Parser rejected input; no behavioral conclusions.", EvidenceIDs: []string{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := s.m.pg.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID) })
	claimed, err := s.m.pg.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != archive.ID {
		t.Fatal(claimed, err)
	}
	snapshot, err := s.m.pg.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["reverse_analyses"] != 1 || snapshot.DataCounts["reverse_analysis_evidence"] != 1 {
		t.Fatal("archive omits reverse analysis")
	}
	if err := s.m.pg.CompleteTaskArchive(archive.ID, snapshot, "test-archive", "fixture", 100, 50); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.m.pg.ListReverseAnalyses(t.Context(), task.ID); err != nil || len(rows) != 0 {
		t.Fatal("hot rows retained", rows, err)
	}
	// A standalone conversation can disappear while the task is archived.
	if _, err := s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, a.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.m.pg.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.State != db.Restoring {
		t.Fatal(claimed, err)
	}
	if _, err := s.m.pg.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	saved, err := s.m.pg.GetReverseAnalysis(t.Context(), task.ID, a.ID)
	if err != nil || saved.Status != "partial" || saved.FactNodeID != node || saved.ConversationID != 0 {
		t.Fatal(saved, err)
	}
	e, err := s.m.pg.ReverseEvidence(t.Context(), a.ID)
	if err != nil || len(e) != 1 || e[0].ID != r.ID || !e[0].IsError {
		t.Fatal(e, err)
	}
}

func TestReverseDeleteDrainIsTaskScoped(t *testing.T) {
	one, cancelOne := context.WithCancelCause(t.Context())
	defer cancelOne(nil)
	two, cancelTwo := context.WithCancelCause(t.Context())
	defer cancelTwo(nil)
	s := &Server{engine: NewEngine(nil), chatCancel: map[string]context.CancelCauseFunc{"conv-1": cancelOne, "conv-2": cancelTwo}, reverseTaskByChat: map[string]string{"conv-1": "7", "conv-2": "8"}}
	s.cancelTaskReverseAnalyses("7")
	if one.Err() == nil || two.Err() != nil {
		t.Fatal("cancellation crossed task ownership")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.waitTaskQuiescent(ctx, "7"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("did not wait for REA cleanup", err)
	}
	s.chatMu.Lock()
	delete(s.reverseTaskByChat, "conv-1")
	s.chatMu.Unlock()
	if err := s.waitTaskQuiescent(t.Context(), "7"); err != nil {
		t.Fatal(err)
	}
	if two.Err() != nil {
		t.Fatal("unrelated analysis was stopped")
	}
}
