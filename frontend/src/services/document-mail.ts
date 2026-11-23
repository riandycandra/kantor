import { authGetJSON, authPostJSON, authRequestJSON, refreshAuthSession } from "@/lib/api-client";
import { env } from "@/lib/env";
import { adminRbacKeys } from "@/services/admin-rbac";
import { getStoredSession } from "@/stores/auth-store";
import type {
  DocumentMailProgressEvent,
  DocumentMailSetting,
  DocumentMailTestResult,
  TestDocumentMailPayload,
  UpdateDocumentMailPayload,
} from "@/types/admin";

export const documentMailKeys = {
  setting: () => [...adminRbacKeys.settings(), "document-mail"] as const,
};

export function getDocumentMailSetting() {
  return authGetJSON<DocumentMailSetting>("/admin/settings/document-mail");
}

export function updateDocumentMailSetting(payload: UpdateDocumentMailPayload) {
  return authRequestJSON<DocumentMailSetting>("/admin/settings/document-mail", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
}

export function sendDocumentMailTest(payload?: TestDocumentMailPayload) {
  return authPostJSON<DocumentMailTestResult, TestDocumentMailPayload | Record<string, never>>(
    "/admin/settings/document-mail/test",
    payload ?? {},
  );
}

async function openProgressStream(
  endpoint: string,
  signal: AbortSignal,
  allowRefreshRetry: boolean,
): Promise<Response> {
  const token = getStoredSession()?.tokens.access_token;
  if (!token) {
    throw new Error("Missing access token");
  }
  const response = await fetch(`${env.VITE_API_BASE_URL}${endpoint}`, {
    method: "GET",
    headers: {
      Accept: "text/event-stream",
      Authorization: `Bearer ${token}`,
    },
    credentials: "include",
    signal,
  });
  if (response.status === 401 && allowRefreshRetry) {
    await refreshAuthSession();
    return openProgressStream(endpoint, signal, false);
  }
  if (!response.ok) {
    throw new Error(`Progress stream failed (${response.status})`);
  }
  return response;
}

function parseSseChunk(chunk: string): DocumentMailProgressEvent | null {
  const lines = chunk.split("\n").map((line) => line.trim()).filter(Boolean);
  if (lines.length === 0 || lines.every((line) => line.startsWith(":"))) {
    return null;
  }
  let eventName = "progress";
  const dataLines: string[] = [];
  for (const line of lines) {
    if (line.startsWith("event:")) {
      eventName = line.slice(6).trim();
    } else if (line.startsWith("data:")) {
      dataLines.push(line.slice(5).trim());
    }
  }
  if (dataLines.length === 0) {
    return null;
  }
  const payload = JSON.parse(dataLines.join("\n")) as Omit<DocumentMailProgressEvent, "event">;
  const normalizedEvent =
    eventName === "completed" || eventName === "failed" ? eventName : "progress";
  return {
    event: normalizedEvent,
    ...payload,
  };
}

export async function streamDocumentMailProgress(
  endpoint: string,
  onEvent: (event: DocumentMailProgressEvent) => void,
  signal: AbortSignal,
): Promise<void> {
  const response = await openProgressStream(endpoint, signal, true);
  if (!response.body) {
    return;
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  while (!signal.aborted) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    buffer += decoder.decode(value, { stream: true }).replaceAll("\r\n", "\n");
    let boundary = buffer.indexOf("\n\n");
    while (boundary !== -1) {
      const chunk = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      const parsed = parseSseChunk(chunk);
      if (parsed) {
        onEvent(parsed);
        if (parsed.event === "completed" || parsed.event === "failed") {
          return;
        }
      }
      boundary = buffer.indexOf("\n\n");
    }
  }
}
