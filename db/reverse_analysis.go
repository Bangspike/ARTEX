package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const ReverseAnalystKey = "reverse_analyst"

var ErrAnalysisBusy = errors.New("逆向分析正在运行，请等待完成后再提交")

type ReverseAnalysis struct {
	ID             string          `json:"id"`
	TaskID         int64           `json:"task_id"`
	ConversationID int64           `json:"conversation_id"`
	Kind           string          `json:"kind"`
	InputPath      string          `json:"input_path"`
	InputSHA256    string          `json:"input_sha256"`
	Objective      string          `json:"objective"`
	Status         string          `json:"status"`
	Summary        string          `json:"summary"`
	Report         string          `json:"report"`
	Limitations    json.RawMessage `json:"limitations"`
	NextSteps      json.RawMessage `json:"next_steps"`
	FactNodeID     int64           `json:"fact_node_id"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

const reverseCols = `id,task_id,COALESCE(conversation_id,0),kind,input_path,input_sha256,objective,status,summary,report,limitations,next_steps,COALESCE(fact_node_id,0),created_at,updated_at`

func scanReverse(row interface{ Scan(...any) error }) (*ReverseAnalysis, error) {
	a := new(ReverseAnalysis)
	err := row.Scan(&a.ID, &a.TaskID, &a.ConversationID, &a.Kind, &a.InputPath, &a.InputSHA256, &a.Objective, &a.Status, &a.Summary, &a.Report, &a.Limitations, &a.NextSteps, &a.FactNodeID, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (d *DB) CreateReverseAnalysis(ctx context.Context, a *ReverseAnalysis) (*Conversation, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Bound heavy native analysis across all callers, including duplicate submits.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741018)`); err != nil {
		return nil, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM reverse_analyses WHERE status IN ('pending','running')`).Scan(&active); err != nil {
		return nil, err
	}
	if active >= 1 {
		return nil, ErrAnalysisBusy
	}
	var taskID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR KEY SHARE`, a.TaskID).Scan(&taskID); err != nil {
		return nil, err
	}
	c, err := scanConv(tx.QueryRowContext(ctx, `INSERT INTO conversations(agent_key,title) VALUES($1,$2) RETURNING `+convCols, ReverseAnalystKey, "逆向分析 · "+a.Kind))
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO reverse_analyses(id,task_id,conversation_id,kind,input_path,input_sha256,objective) VALUES($1,$2,$3,$4,$5,$6,$7)`, a.ID, a.TaskID, c.ID, a.Kind, a.InputPath, a.InputSHA256, a.Objective)
	if err != nil {
		return nil, err
	}
	a.ConversationID = c.ID
	return &c, tx.Commit()
}

func (d *DB) ReverseAnalysisForConversation(ctx context.Context, id int64) (*ReverseAnalysis, error) {
	return scanReverse(d.QueryRowContext(ctx, `SELECT `+reverseCols+` FROM reverse_analyses WHERE conversation_id=$1`, id))
}
func (d *DB) GetReverseAnalysis(ctx context.Context, taskID int64, id string) (*ReverseAnalysis, error) {
	return scanReverse(d.QueryRowContext(ctx, `SELECT `+reverseCols+` FROM reverse_analyses WHERE task_id=$1 AND id=$2`, taskID, id))
}
func (d *DB) ListReverseAnalyses(ctx context.Context, taskID int64) ([]*ReverseAnalysis, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+reverseCols+` FROM reverse_analyses WHERE task_id=$1 ORDER BY created_at DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReverseAnalysis{}
	for rows.Next() {
		a, e := scanReverse(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (d *DB) StartReverseAnalysis(ctx context.Context, conv int64) (bool, error) {
	r, err := d.ExecContext(ctx, `UPDATE reverse_analyses SET status='running',updated_at=now() WHERE conversation_id=$1 AND status='pending'`, conv)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (d *DB) FinishReverseAnalysis(conv int64, status, reason string) error {
	if status != "failed" && status != "stopped" {
		return errors.New("invalid terminal status")
	}
	_, err := d.Exec(`UPDATE reverse_analyses SET status=$2,summary=$3,updated_at=now() WHERE conversation_id=$1 AND status IN ('pending','running')`, conv, status, reason)
	return err
}

type ReverseEvidence struct {
	ID            string          `json:"id"`
	AnalysisID    string          `json:"analysis_id"`
	Tool          string          `json:"tool"`
	Arguments     json.RawMessage `json:"arguments"`
	Path          string          `json:"path"`
	SHA256        string          `json:"sha256"`
	REAEvidenceID string          `json:"rea_evidence_id"`
	IsError       bool            `json:"is_error"`
}

func (d *DB) AddReverseEvidence(ctx context.Context, e ReverseEvidence) error {
	r, err := d.ExecContext(ctx, `INSERT INTO reverse_analysis_evidence(id,analysis_id,tool,arguments,path,sha256,rea_evidence_id,is_error)
 SELECT $1,id,$3,$4,$5,$6,$7,$8 FROM reverse_analyses WHERE id=$2 AND status='running'`, e.ID, e.AnalysisID, e.Tool, string(e.Arguments), e.Path, e.SHA256, e.REAEvidenceID, e.IsError)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("analysis is not running")
	}
	return err
}
func (d *DB) ReverseEvidence(ctx context.Context, id string) ([]ReverseEvidence, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,analysis_id,tool,arguments,path,sha256,rea_evidence_id,is_error FROM reverse_analysis_evidence WHERE analysis_id=$1 ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReverseEvidence{}
	for rows.Next() {
		var e ReverseEvidence
		if err = rows.Scan(&e.ID, &e.AnalysisID, &e.Tool, &e.Arguments, &e.Path, &e.SHA256, &e.REAEvidenceID, &e.IsError); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type ReverseConclusion struct {
	Status      string   `json:"status"`
	Summary     string   `json:"summary"`
	Report      string   `json:"report"`
	Limitations []string `json:"limitations,omitempty"`
	NextSteps   []string `json:"next_steps,omitempty"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// CompleteReverseAnalysis commits the report and one task fact atomically. The
// conversation binding owns the destination; no caller-supplied task ID is used.
func (d *DB) CompleteReverseAnalysis(ctx context.Context, conv int64, c ReverseConclusion) (int64, error) {
	if (c.Status != "completed" && c.Status != "partial") || strings.TrimSpace(c.Summary) == "" || strings.TrimSpace(c.Report) == "" || len(c.EvidenceIDs) == 0 {
		return 0, errors.New("status、summary、report、evidence_ids 必填")
	}
	if len(c.Report) > 200000 || len(c.Summary) > 4000 {
		return 0, errors.New("report or summary too large")
	}
	if c.Limitations == nil {
		c.Limitations = []string{}
	}
	if c.NextSteps == nil {
		c.NextSteps = []string{}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	a, err := scanReverse(tx.QueryRowContext(ctx, `SELECT `+reverseCols+` FROM reverse_analyses WHERE conversation_id=$1 FOR UPDATE`, conv))
	if err != nil {
		return 0, err
	}
	if a.Status != "running" {
		return 0, errors.New("analysis is not running or already sealed")
	}
	var refs []string
	for _, id := range c.EvidenceIDs {
		var tool, sha string
		if err = tx.QueryRowContext(ctx, `SELECT tool,sha256 FROM reverse_analysis_evidence WHERE id=$1 AND analysis_id=$2`, id, a.ID).Scan(&tool, &sha); err != nil {
			return 0, fmt.Errorf("evidence does not belong to this analysis: %s", id)
		}
		refs = append(refs, fmt.Sprintf("%s · %s · sha256:%s", id, tool, sha))
	}
	if c.Status == "completed" {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM reverse_analysis_evidence WHERE analysis_id=$1 AND NOT is_error AND tool=$2`, a.ID, map[string]string{"javascript": "analyze_javascript_application", "native": "binary_overview"}[a.Kind]).Scan(&count); err != nil {
			return 0, err
		}
		if count == 0 {
			return 0, errors.New("缺少成功的目标分析证据")
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM reverse_analysis_evidence bundle
WHERE bundle.analysis_id=$1 AND NOT bundle.is_error AND bundle.tool='export_evidence_bundle'
AND NOT EXISTS (SELECT 1 FROM reverse_analysis_evidence newer WHERE newer.analysis_id=$1
 AND NOT newer.is_error AND newer.tool NOT IN ('export_evidence_bundle','binary_session','close_binary')
 AND newer.created_at>bundle.created_at)`, a.ID).Scan(&count); err != nil {
			return 0, err
		}
		if count == 0 {
			return 0, errors.New("需要在最后一次分析之后导出完整 Evidence bundle；若已导出后又补充了分析，请以 partial 提交并重新发起补充分析")
		}
	}
	var expID int64
	if err = tx.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR KEY SHARE`, a.TaskID).Scan(&expID); err != nil {
		return 0, err
	}
	report := c.Report + "\n\n## 分析来源\n\n输入 SHA-256: `" + a.InputSHA256 + "`\n\n- " + strings.Join(refs, "\n- ") + "\n"
	payload, _ := json.Marshal(map[string]any{"summary": c.Summary, "detail": report, "confidence": "inferred", "evidence": strings.Join(refs, "; "), "reverse_analysis_id": a.ID, "limitations": c.Limitations, "next_steps": c.NextSteps})
	var node int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,'fact',$2,5,'confirmed','reverse_analyst') RETURNING id`, expID, string(payload)).Scan(&node); err != nil {
		return 0, err
	}
	limits, _ := json.Marshal(c.Limitations)
	next, _ := json.Marshal(c.NextSteps)
	_, err = tx.ExecContext(ctx, `UPDATE reverse_analyses SET status=$2,summary=$3,report=$4,limitations=$5,next_steps=$6,fact_node_id=$7,updated_at=now() WHERE id=$1`, a.ID, c.Status, c.Summary, report, string(limits), string(next), node)
	if err != nil {
		return 0, err
	}
	return node, tx.Commit()
}

// Startup cannot resume an old in-memory Agent run. Preserve its evidence and
// explicitly mark interrupted work rather than declaring it complete.
func (d *DB) RecoverReverseAnalyses() error {
	_, err := d.Exec(`UPDATE reverse_analyses SET status='stopped',summary='服务重启，分析会话已中断；已保存的证据仍可读取。',updated_at=now() WHERE status IN ('pending','running')`)
	return err
}
