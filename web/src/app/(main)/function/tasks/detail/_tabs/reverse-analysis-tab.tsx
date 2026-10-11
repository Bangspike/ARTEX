"use client";

import * as React from "react";

import { toast } from "sonner";

import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { api, type ReverseAnalysis, type ReverseEvidence } from "@/lib/api";

const statusNames: Record<ReverseAnalysis["status"], string> = {
  pending: "等待启动",
  running: "分析中",
  completed: "已完成",
  partial: "部分完成",
  failed: "失败",
  stopped: "已停止",
};

export function ReverseAnalysisTab({ taskId }: { taskId: string }) {
  const [items, setItems] = React.useState<ReverseAnalysis[]>([]);
  const [kind, setKind] = React.useState<"javascript" | "native">("javascript");
  const [path, setPath] = React.useState("");
  const [objective, setObjective] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [selected, setSelected] = React.useState<ReverseAnalysis | null>(null);
  const [evidence, setEvidence] = React.useState<ReverseEvidence[]>([]);
  const selectedId = selected?.id;
  const refresh = React.useCallback(async () => {
    try {
      setItems((await api.reverseAnalyses(taskId)).analyses);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "读取分析失败");
    }
  }, [taskId]);
  React.useEffect(() => {
    void refresh();
  }, [refresh]);
  const active = items.some((i) => i.status === "pending" || i.status === "running");
  React.useEffect(() => {
    if (!active) return;
    const timer = setInterval(() => void refresh(), 4000);
    return () => clearInterval(timer);
  }, [active, refresh]);
  React.useEffect(() => {
    if (!selectedId || !items.some((item) => item.id === selectedId)) return;
    let cancelled = false;
    void api
      .reverseAnalysis(taskId, selectedId)
      .then((result) => {
        if (!cancelled) {
          setSelected(result.analysis);
          setEvidence(result.evidence);
        }
      })
      .catch((e) => {
        if (!cancelled) toast.error(e instanceof Error ? e.message : "读取失败");
      });
    return () => {
      cancelled = true;
    };
  }, [items, taskId, selectedId]);
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await api.startReverseAnalysis(taskId, { kind, input_path: path.trim(), objective: objective.trim() });
      toast.success("逆向分析已提交");
      await refresh();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "提交失败");
    } finally {
      setBusy(false);
    }
  }
  async function stop(id: string) {
    try {
      await api.stopReverseAnalysis(taskId, id);
      await refresh();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "停止失败");
    }
  }
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>逆向分析</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="mb-4 text-muted-foreground text-sm">
            选择工作空间内的材料并描述需要回答的问题。完成后，报告与证据将关联本任务，分析结论写入探索链路。
          </p>
          <form onSubmit={submit} className="space-y-3">
            <label className="block text-sm">
              材料类型
              <select
                value={kind}
                onChange={(e) => setKind(e.target.value as "javascript" | "native")}
                className="mt-1 block w-full rounded-md border bg-background p-2"
              >
                <option value="javascript">JavaScript / Electron（目录或 ASAR）</option>
                <option value="native">原生二进制（Ghidra）</option>
              </select>
            </label>
            <label className="block text-sm" htmlFor="reverse-input-path">
              材料路径
              <Input
                id="reverse-input-path"
                className="mt-1"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="工作空间相对路径，例如 samples/app.asar"
                required
              />
            </label>
            <label className="block text-sm" htmlFor="reverse-objective">
              分析目标
              <Textarea
                id="reverse-objective"
                className="mt-1"
                value={objective}
                onChange={(e) => setObjective(e.target.value)}
                placeholder="例如：定位登录请求的构造位置，说明参数来源并引用证据。"
                required
              />
            </label>
            <Button type="submit" disabled={busy || active}>
              {busy ? "正在提交…" : "开始分析"}
            </Button>
          </form>
        </CardContent>
      </Card>
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div className="flex justify-end">
        <Button variant="outline" onClick={() => void refresh()}>
          刷新
        </Button>
      </div>
      {!error && items.length === 0 && <p className="text-muted-foreground text-sm">此任务暂无逆向分析。</p>}
      {items.map((item) => (
        <Card key={item.id}>
          <CardContent className="space-y-2 pt-4">
            <div className="flex items-center justify-between gap-3">
              <span className="font-medium">{item.objective}</span>
              <span className="shrink-0 text-sm">{statusNames[item.status]}</span>
            </div>
            <p className="text-muted-foreground text-sm">
              {item.summary || (item.status === "running" ? "正在收集分析证据…" : "报告尚未生成")}
            </p>
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  if (selectedId !== item.id) {
                    setSelected(item);
                    setEvidence([]);
                  } else {
                    void refresh();
                  }
                }}
              >
                报告与证据
              </Button>
              {(item.status === "pending" || item.status === "running") && (
                <Button size="sm" variant="outline" onClick={() => void stop(item.id)}>
                  停止
                </Button>
              )}
            </div>
          </CardContent>
        </Card>
      ))}
      {selected && (
        <Card>
          <CardHeader>
            <CardTitle>分析报告</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <p className="break-all text-muted-foreground text-xs">输入 SHA-256：{selected.input_sha256}</p>
            {selected.report ? (
              <Markdown text={selected.report} />
            ) : (
              <p>{selected.summary || "报告尚未提交，可先查看已保留的证据。"}</p>
            )}
            {(selected.limitations?.length ?? 0) > 0 && (
              <div>
                <strong>局限</strong>
                <ul className="list-disc pl-5">
                  {[...new Set(selected.limitations)].map((s) => (
                    <li key={s}>{s}</li>
                  ))}
                </ul>
              </div>
            )}
            {(selected.next_steps?.length ?? 0) > 0 && (
              <div>
                <strong>后续验证</strong>
                <ul className="list-disc pl-5">
                  {[...new Set(selected.next_steps)].map((s) => (
                    <li key={s}>{s}</li>
                  ))}
                </ul>
              </div>
            )}
            <div className="space-y-2">
              {evidence.map((e) => (
                <div key={e.id} className="flex items-center justify-between gap-2 text-sm">
                  <span className="break-all">
                    {e.tool}
                    {e.is_error ? "（错误证据）" : ""} · {e.id}
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() =>
                      void api
                        .downloadReverseEvidence(taskId, selected.id, e.id)
                        .catch((err) => toast.error(err.message))
                    }
                  >
                    下载 JSON
                  </Button>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
