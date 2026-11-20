export type ToastKind = "ok" | "err" | "warn";

const ERR_TOAST_RE =
  /(HTTP \d{3}|fail|error|invalid|denied|forbid|unauthorized|too many|rate limit|too large|unsupported|mismatch|expired|aborted|timeout|cannot|unable|失败|错误|无效|拒绝|无法|未能|不可用|受限|异常|超时|必填|不能为空|未登录|未授权|权限不足|频繁|过大|不支持|不匹配|过期|中断|失敗|錯誤|無效|拒絕|無法|異常|逾時|過大|頻繁|不支援|未授權|權限不足)/i;

const WARN_TOAST_RE = /(not detected|未检测到|未偵測到)/i;

function hasNoDeviceOnSameLine(msg: string): boolean {
  const lower = msg.toLowerCase();
  let pos = 0;
  while (true) {
    const noIdx = lower.indexOf("no ", pos);
    if (noIdx < 0) return false;
    const devIdx = lower.indexOf(" device", noIdx + 3);
    if (devIdx < 0) return false;
    let lineBreak = -1;
    for (let i = devIdx - 1; i >= noIdx + 3; i--) {
      const c = lower.charCodeAt(i);
      if (c === 10 || c === 13 || c === 0x2028 || c === 0x2029) {
        lineBreak = i;
        break;
      }
    }
    if (lineBreak < 0) return true;
    pos = lineBreak + 1;
  }
}

export function trimEndChars(s: string, chars: string, trimStart?: boolean): string {
  let a = 0;
  let b = s.length;
  if (trimStart) while (a < b && chars.includes(s[a])) a++;
  while (b > a && chars.includes(s[b - 1])) b--;
  return a || b < s.length ? s.slice(a, b) : s;
}

export function extractColonDetail(trimmed: string): [string, string] | null {
  const c = trimmed.indexOf(":");
  if (c <= 0) return null;
  let h = trimmed.slice(0, c).toLowerCase();
  if (/^validation\s+failed$/.test(h)) h = "validation";
  const d = trimmed.slice(c + 1).trimStart();
  return !d || /[\n\r\u2028\u2029]/.test(d) ? null : [h, d];
}

export function inferToastKind(msg: string): ToastKind {
  if (ERR_TOAST_RE.test(msg)) {
    return "err";
  }
  if (WARN_TOAST_RE.test(msg) || hasNoDeviceOnSameLine(msg)) {
    return "warn";
  }
  return "ok";
}
