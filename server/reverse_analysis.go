package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/mcpstdio"
	"github.com/Autumn-27/artex/reanalysis"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/google/uuid"
)

type reverseRequest struct {
	Kind      string `json:"kind"`
	InputPath string `json:"input_path"`
	Objective string `json:"objective"`
}

func (s *Server) startReverseAnalysis(ctx context.Context, taskID int64, req reverseRequest) (*db.ReverseAnalysis, error) {
	if req.Kind != "javascript" && req.Kind != "native" {
		return nil, errors.New("kind 必须为 javascript 或 native")
	}
	if strings.TrimSpace(req.Objective) == "" || len(req.Objective) > 16000 {
		return nil, errors.New("请提供分析目标（最多 16000 字节）")
	}
	if _, ok := s.m.Task(strconv.FormatInt(taskID, 10)); !ok {
		return nil, errors.New("任务不存在或已归档")
	}
	m, err := s.findMCPByName("rea")
	if err != nil {
		return nil, err
	}
	if m == nil || !m.Enabled || m.Transport != "stdio" || m.Command == "" {
		return nil, errors.New("请先配置并启用名为 rea 的 stdio MCP")
	}
	a, err := s.m.pg.GetAgentByKey(db.ReverseAnalystKey)
	if err != nil {
		return nil, err
	}
	if a == nil || !a.Enabled {
		return nil, errors.New("逆向分析 Agent 未启用")
	}
	for _, key := range []string{"reverse_analysis_context", "complete_reverse_analysis"} {
		t, e := s.m.pg.GetTool(key)
		if e != nil {
			return nil, e
		}
		if t == nil || !t.Enabled || !contains(t.Agents, a.Key) {
			return nil, fmt.Errorf("请为逆向 Agent 绑定工具 %s", key)
		}
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: a.Key}) == nil {
		return nil, errors.New(s.chatUnavailableReason())
	}
	if err = s.ctx.Err(); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	root := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(taskID, 10), "reverse", id)
	input, digest, err := reanalysis.Snapshot(s.m.dir, req.InputPath, filepath.Join(root, "input"))
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if req.Kind == "native" {
		info, e := os.Stat(input)
		if e != nil || info.IsDir() {
			_ = os.RemoveAll(root)
			return nil, errors.New("native 分析需要单个二进制文件")
		}
	}
	job := &db.ReverseAnalysis{ID: id, TaskID: taskID, Kind: req.Kind, InputPath: input, InputSHA256: digest, Objective: req.Objective, Status: "pending"}
	// Serialize admission with the existing task delete/archive barrier. Copying
	// happens outside this lock so large inputs cannot stall unrelated tasks.
	s.concMu.Lock()
	defer s.concMu.Unlock()
	s.chatMu.Lock()
	active := len(s.reverseTaskByChat) > 0
	s.chatMu.Unlock()
	if active {
		_ = os.RemoveAll(root)
		return nil, db.ErrAnalysisBusy
	}
	if s.engine != nil && s.engine.IsDeleting(strconv.FormatInt(taskID, 10)) {
		_ = os.RemoveAll(root)
		return nil, errors.New("任务正在删除或归档")
	}
	conv, err := s.m.pg.CreateReverseAnalysis(ctx, job)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	busy := s.convBusyKey(conv.ID)
	s.chatMu.Lock()
	s.chatBusy[busy] = true
	if s.reverseTaskByChat == nil {
		s.reverseTaskByChat = map[string]string{}
	}
	s.reverseTaskByChat[busy] = strconv.FormatInt(taskID, 10)
	s.chatMu.Unlock()
	s.runConversation(conv, "请执行本会话绑定的逆向分析。加载 rea-analysis Skill，然后调用 reverse_analysis_context 获取目标和材料。分析完成后用 complete_reverse_analysis 写回。", busy)
	return job, nil
}

func (s *Server) createReverseAnalysis(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		return
	}
	var req reverseRequest
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	a, err := s.startReverseAnalysis(r.Context(), id, req)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 202, a)
}
func (s *Server) listReverseAnalyses(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		return
	}
	items, err := s.m.pg.ListReverseAnalyses(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"analyses": items})
}
func (s *Server) reverseAnalysisDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		return
	}
	a, err := s.m.pg.GetReverseAnalysis(r.Context(), id, r.PathValue("analysis"))
	if err != nil {
		writeErr(w, 404, "分析不存在")
		return
	}
	evidence, err := s.m.pg.ReverseEvidence(r.Context(), a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"analysis": a, "evidence": evidence})
}
func (s *Server) reverseEvidenceDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		return
	}
	a, err := s.m.pg.GetReverseAnalysis(r.Context(), id, r.PathValue("analysis"))
	if err != nil {
		writeErr(w, 404, "分析不存在")
		return
	}
	evidence, err := s.m.pg.ReverseEvidence(r.Context(), a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, e := range evidence {
		if e.ID == r.PathValue("evidence") {
			path := filepath.Join(s.m.dir, e.Path)
			digest, err := reanalysis.HashFile(path)
			if err != nil || digest != e.SHA256 {
				writeErr(w, 409, "证据文件缺失或哈希校验失败")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", `attachment; filename="`+e.ID+`.json"`)
			http.ServeFile(w, r, path)
			return
		}
	}
	writeErr(w, 404, "证据不存在")
}
func (s *Server) stopReverseAnalysis(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		return
	}
	a, err := s.m.pg.GetReverseAnalysis(r.Context(), id, r.PathValue("analysis"))
	if err != nil {
		writeErr(w, 404, "分析不存在")
		return
	}
	s.chatMu.Lock()
	cancel := s.chatCancel[s.convBusyKey(a.ConversationID)]
	s.chatMu.Unlock()
	if cancel != nil {
		cancel(agent.AbortChatStoppedByUser)
	}
	if err = s.m.pg.FinishReverseAnalysis(a.ConversationID, "stopped", "用户停止分析"); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"stopped": true})
}

func (s *Server) reverseAnalysisTools() []actool.CoreTool {
	return []actool.CoreTool{
		wrTool("start_reverse_analysis", "为 ARTEX 任务启动独立逆向分析会话（JS/Electron 或 Ghidra）。立即返回分析 ID；用 get_reverse_analysis 查询结果。材料路径相对于 ARTEX 工作空间。", objSchema(map[string]any{"task_id": map[string]any{"type": "integer"}, "kind": map[string]any{"type": "string", "enum": []string{"javascript", "native"}}, "input_path": strParam("工作空间内的文件或目录，相对路径"), "objective": strParam("需要分析回答的问题")}, "task_id", "kind", "input_path", "objective"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var req struct {
				reverseRequest
				TaskID int64 `json:"task_id"`
			}
			if err := json.Unmarshal(in, &req); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if ri := agent.RunInfoFrom(ctx); ri.TaskID > 0 && ri.TaskID != req.TaskID {
				return actool.Errorf("不能从当前任务向其他任务提交分析"), nil
			}
			a, err := s.startReverseAnalysis(ctx, req.TaskID, req.reverseRequest)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(a)
		}),
		roTool("get_reverse_analysis", "读取任务关联的逆向分析状态、报告与证据引用。", objSchema(map[string]any{"task_id": map[string]any{"type": "integer"}, "analysis_id": strParam("分析 ID")}, "task_id", "analysis_id"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID int64  `json:"task_id"`
				ID     string `json:"analysis_id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			job, err := s.m.pg.GetReverseAnalysis(ctx, a.TaskID, a.ID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(job)
		}),
		roTool("reverse_analysis_context", "获取本逆向会话绑定的任务、材料快照、目标、导出路径和已保存的证据。", objSchema(nil), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
			a, err := s.m.pg.ReverseAnalysisForConversation(ctx, intercept.ConvIDFromContext(ctx))
			if err != nil {
				return actool.Errorf("当前会话没有绑定逆向分析"), nil
			}
			e, err := s.m.pg.ReverseEvidence(ctx, a.ID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{"analysis": a, "evidence_export_path": s.reverseExportPath(a), "evidence": e})
		}),
		wrTool("complete_reverse_analysis", "提交逆向分析报告，并把一条带证据的分析结论写回原任务。目标任务由会话绑定；静态推断不作为已验证漏洞。", objSchema(map[string]any{"status": map[string]any{"type": "string", "enum": []string{"completed", "partial"}}, "summary": strParam("结论摘要"), "report": strParam("Markdown 报告，区分观察、推断与未知"), "limitations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "next_steps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "status", "summary", "report", "evidence_ids"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var c db.ReverseConclusion
			if err := json.Unmarshal(in, &c); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			conv := intercept.ConvIDFromContext(ctx)
			node, err := s.m.pg.CompleteReverseAnalysis(ctx, conv, c)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			a, err := s.m.pg.ReverseAnalysisForConversation(ctx, conv)
			if err == nil {
				if t, ok := s.m.Task(strconv.FormatInt(a.TaskID, 10)); ok {
					t.NotifyHint([]string{"逆向分析已写回事实节点 " + strconv.FormatInt(node, 10) + "：" + c.Summary})
				}
			}
			return jsonResult(map[string]any{"saved": true, "fact_node_id": node})
		}),
	}
}

func (s *Server) reverseExportPath(a *db.ReverseAnalysis) string {
	return filepath.Join(s.m.dir, "tasks", strconv.FormatInt(a.TaskID, 10), "reverse", a.ID, "evidence-bundle.json")
}

func (s *Server) connectAgentMCP(ctx context.Context, m *db.MCPServer, key string) (mcpClient, error) {
	if key != db.ReverseAnalystKey || m.Name != "rea" {
		return connectMCP(ctx, m)
	}
	a, err := s.m.pg.ReverseAnalysisForConversation(ctx, intercept.ConvIDFromContext(ctx))
	if err != nil || a.Status != "running" {
		return nil, errors.New("REA requires an active task-bound analysis")
	}
	if m.Transport != "stdio" {
		return nil, errors.New("REA v1 requires stdio")
	}
	c, err := mcpstdio.New(ctx, m.Name, m.Command, jsonStrMap(m.Env), jsonStrSlice(m.Args)...)
	if err != nil {
		return nil, err
	}
	c.Hooks = mcpstdio.Hooks{Allow: reanalysis.AllowedTool, Before: func(callctx context.Context, tool string, in json.RawMessage) (json.RawMessage, error) {
		live, err := s.m.pg.ReverseAnalysisForConversation(callctx, a.ConversationID)
		if err != nil || live.Status != "running" {
			return nil, errors.New("分析已结束")
		}
		var args map[string]any
		if err = json.Unmarshal(in, &args); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]any{}
		}
		switch tool {
		case "open_binary":
			if a.Kind != "native" || args["path"] != a.InputPath {
				return nil, errors.New("只能打开本任务绑定的原生材料")
			}
			args["provider_id"] = "ghidra"
			delete(args, "snapshot_path")
		case "analyze_javascript_application":
			if a.Kind != "javascript" || args["input_path"] != a.InputPath {
				return nil, errors.New("只能分析本任务绑定的 JS/Electron 材料")
			}
			args["detail"] = "summary"
		case "export_evidence_bundle":
			args = map[string]any{"path": s.reverseExportPath(a), "overwrite": false}
		case "close_binary":
			args = map[string]any{}
		}
		return json.Marshal(args)
	}, After: func(callctx context.Context, tool string, args, raw json.RawMessage) (string, error) {
		return s.retainReverseEvidence(callctx, a, tool, args, raw)
	}}
	return c, nil
}

func (s *Server) retainReverseEvidence(ctx context.Context, a *db.ReverseAnalysis, tool string, args, raw json.RawMessage) (string, error) {
	var result struct {
		IsError    bool `json:"isError"`
		Structured struct {
			EvidenceID string `json:"evidence_id"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	id := uuid.NewString()
	rel := filepath.Join("tasks", strconv.FormatInt(a.TaskID, 10), "reverse", a.ID, "evidence", id+".json")
	path := filepath.Join(s.m.dir, rel)
	sha, err := reanalysis.WriteExclusive(path, raw)
	if err != nil {
		return "", err
	}
	if tool == "export_evidence_bundle" && !result.IsError {
		// Retain the full exported bundle as the evidence download, not just its receipt.
		path = s.reverseExportPath(a)
		sha, err = reanalysis.HashFile(path)
		if err != nil {
			return "", err
		}
		rel, _ = filepath.Rel(s.m.dir, path)
	}
	e := db.ReverseEvidence{ID: id, AnalysisID: a.ID, Tool: tool, Arguments: args, Path: rel, SHA256: sha, REAEvidenceID: result.Structured.EvidenceID, IsError: result.IsError}
	if err = s.m.pg.AddReverseEvidence(ctx, e); err != nil {
		return "", err
	}
	receipt, _ := json.Marshal(map[string]any{"artex_evidence_id": id, "sha256": sha, "analysis_id": a.ID, "is_error": e.IsError})
	return string(receipt), nil
}

const reverseAnalystPrompt = `你是 ARTEX 的逆向分析 Agent。执行当前会话绑定的分析目标。先加载 rea-analysis Skill，然后调用 reverse_analysis_context。使用 REA 分析 JS/Electron 或 Ghidra 原生材料；只分析绑定材料。按需查询，不把静态推断当成已验证漏洞。输出的每个关键结论应关联保存的证据。导出完整 evidence bundle，再调用 complete_reverse_analysis 写回报告和后续线索。使用上下文返回的 ARTEX evidence IDs（而不是 REA 内部 ev_ ID）作为 evidence_ids。无法完成时保留错误证据并以 partial 报告局限。完成后停止。`

func (s *Server) seedReverseAnalyst() error {
	pg := s.m.pg
	for _, t := range s.reverseAnalysisTools() {
		bindings := []string{db.ReverseAnalystKey}
		if t.Name() == "start_reverse_analysis" || t.Name() == "get_reverse_analysis" {
			bindings = []string{"auto", "planner", "worker", "mainagent"}
		}
		schema, _ := json.Marshal(t.InputSchema())
		agents, _ := json.Marshal(bindings)
		if err := pg.SeedTool(t.Name(), t.Description(), schema, agents); err != nil {
			return err
		}
	}
	tx, err := pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(7337741019)`); err != nil {
		return err
	}
	var flag string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='reverse_analyst_seed_v1'`).Scan(&flag)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if flag == "true" {
		return nil
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled) VALUES($1,'逆向分析','分析 JS/Electron 与原生文件，并将证据和报告写回任务。','assistant',false,true) ON CONFLICT(key) DO NOTHING RETURNING id`, db.ReverseAnalystKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id > 0 {
		var pid int64
		if err = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) VALUES($1,1,$2,'内置默认','system') RETURNING id`, id, reverseAnalystPrompt).Scan(&pid); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, id); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES($1,'rea-analysis',true) ON CONFLICT DO NOTHING`, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO mcp_servers(name,transport,command,args,env,enabled) SELECT 'rea','stdio','rea','["mcp"]','{}',false WHERE NOT EXISTS(SELECT 1 FROM mcp_servers WHERE name='rea')`); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES('reverse_analyst_seed_v1','true') ON CONFLICT(key) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) finishReverseConversation(conv int64, ctx context.Context, failure string) {
	status, reason := "failed", "Agent 已结束，但尚未提交分析报告"
	if failure != "" {
		reason = "分析失败：" + failure
	}
	if ctx.Err() != nil {
		status, reason = "stopped", "分析已取消或运行预算耗尽"
	}
	if err := s.m.pg.FinishReverseAnalysis(conv, status, reason); err != nil {
		log.Printf("[reverse] finish: %v", err)
	}
}

func (s *Server) cancelTaskReverseAnalyses(taskID string) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	for key, owner := range s.reverseTaskByChat {
		if owner == taskID {
			if cancel := s.chatCancel[key]; cancel != nil {
				cancel(agent.AbortTaskDeleted)
			}
		}
	}
}
