#!/bin/bash
# 等 renew 任务（proc_7eaad4dc1bad）彻底结束 + 工作区干净后，串行启动 pipepurge
while pgrep -f "claude --model" >/dev/null; do sleep 60; done
sleep 30  # 进程退出后 tee 落盘缓冲
cd /root/mimo2api
if ! git diff --quiet || [ -n "$(git status --porcelain | grep -v '^?? \.env$')" ]; then
  echo "WORKTREE_DIRTY — 有未落库的改动（renew 任务可能没收尾干净），中止启动，等待人工检查"
  exit 1
fi
echo "WORKTREE_CLEAN — $(git log --oneline -1) — 启动 pipepurge"
CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 claude --model dashscope/glm-5.3 -p "$(cat /root/artex-fork-review/TASK_MIMO_PIPEPURGE.md)" 2>&1 | tee /root/artex-fork-review/mimo-pipepurge-run.log | tail -5
