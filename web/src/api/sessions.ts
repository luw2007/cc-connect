import api from './client';

export interface LastMessage {
  role: string;
  content: string;
  timestamp: string;
}

export interface Session {
  id: string;
  session_key: string;
  name: string;
  platform: string;
  agent_type: string;
  active: boolean;
  live: boolean;
  created_at: string;
  updated_at: string;
  history_count: number;
  last_message: LastMessage | null;
  user_name?: string;
  chat_name?: string;
}

export interface SessionDetail extends Session {
  agent_session_id: string;
  history: { role: string; content: string; timestamp: string }[];
}

export interface AgentSessionView {
  id: string;
  summary?: string;
  message_count: number;
  modified_at?: string;
  time_source: 'record' | 'unknown';
  project_path?: string;
  agent_type: string;
  bound: boolean;
  bound_chat_id?: string;
  cc_session_id?: string;
}

export interface AgentSessionListParams {
  since?: string;
  until?: string;
  limit?: number;
  scope?: 'project' | 'all';
}

export interface AgentSessionListResponse {
  sessions: AgentSessionView[];
  count: number;
  scope: 'project' | 'all';
  all_sessions_supported: boolean;
  filtered_unknown_time: number;
}

export interface AgentSessionHistoryEntry {
  role: string;
  content: string;
  timestamp: string;
}

export interface AgentSessionHistoryResponse {
  id: string;
  agent_type: string;
  history: AgentSessionHistoryEntry[];
  count: number;
}

export interface CreateAgentSessionGroupBody {
  owner_user_id: string;
  name?: string;
}

export interface CreateAgentSessionGroupResponse {
  created: boolean;
  chat_id: string;
  owner_user_id: string;
  binding_namespace: string;
}

export const listSessions = (project: string) =>
  api.get<{ sessions: Session[]; active_keys: Record<string, string> }>(`/projects/${project}/sessions`);
export const getSession = (project: string, id: string, historyLimit?: number) =>
  api.get<SessionDetail>(`/projects/${project}/sessions/${id}`, historyLimit ? { history_limit: String(historyLimit) } : undefined);
export const createSession = (project: string, body: { session_key: string; name?: string }) =>
  api.post(`/projects/${project}/sessions`, body);
export const deleteSession = (project: string, id: string) => api.delete(`/projects/${project}/sessions/${id}`);
export const switchSession = (project: string, body: { session_key: string; session_id: string }) =>
  api.post(`/projects/${project}/sessions/switch`, body);
export const sendMessage = (project: string, body: { session_key: string; message: string }) =>
  api.post(`/projects/${project}/send`, body);

export const listAgentSessions = (project: string, params: AgentSessionListParams) => {
  const query: Record<string, string> = {};
  if (params.since) query.since = params.since;
  if (params.until) query.until = params.until;
  if (params.limit !== undefined) query.limit = String(params.limit);
  if (params.scope) query.scope = params.scope;
  return api.get<AgentSessionListResponse>(`/projects/${project}/agent-sessions`, query);
};

export const getAgentSessionHistory = (project: string, id: string, limit?: number) =>
  api.get<AgentSessionHistoryResponse>(
    `/projects/${project}/agent-sessions/${encodeURIComponent(id)}/history`,
    limit !== undefined ? { limit: String(limit) } : undefined,
  );

export const createAgentSessionGroup = (project: string, id: string, body: CreateAgentSessionGroupBody) =>
  api.post<CreateAgentSessionGroupResponse>(`/projects/${project}/agent-sessions/${encodeURIComponent(id)}/group`, body);
