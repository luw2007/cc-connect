import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type MouseEvent } from 'react';
import { AlertCircle, Bot, ChevronDown, Clock, History, MessageSquare, Users } from 'lucide-react';
import { ApiError } from '@/api';
import {
  createAgentSessionGroup,
  getAgentSessionHistory,
  listAgentSessions,
  type AgentSessionHistoryResponse,
  type AgentSessionListResponse,
  type AgentSessionView,
  type CreateAgentSessionGroupResponse,
} from '@/api/sessions';
import type { ProjectSummary } from '@/api/projects';
import { Badge, Button, EmptyState, Input, Modal } from '@/components/ui';
import { cn } from '@/lib/utils';

type Scope = 'project' | 'all';
type TimeRange = 'today' | '7d' | '30d' | 'all' | 'custom';

interface AgentSessionListProps {
  projects: ProjectSummary[];
}

interface HistoryState {
  loading: boolean;
  data?: AgentSessionHistoryResponse;
  error?: string;
  unsupported?: boolean;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function toRFC3339(value: string): string | undefined {
  if (!value) return undefined;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

function rangeParams(range: TimeRange, customSince: string, customUntil: string) {
  if (range === 'all') return {};
  if (range === 'custom') {
    return {
      since: toRFC3339(customSince),
      until: toRFC3339(customUntil),
    };
  }

  const until = new Date();
  const since = new Date(until);
  if (range === 'today') {
    since.setHours(0, 0, 0, 0);
  } else {
    since.setDate(since.getDate() - (range === '7d' ? 7 : 30));
  }
  return { since: since.toISOString(), until: until.toISOString() };
}

function formatTimestamp(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

export default function AgentSessionList({ projects }: AgentSessionListProps) {
  const [project, setProject] = useState('');
  const [scope, setScope] = useState<Scope>('project');
  const [timeRange, setTimeRange] = useState<TimeRange>('7d');
  const [customSince, setCustomSince] = useState('');
  const [customUntil, setCustomUntil] = useState('');
  const [result, setResult] = useState<AgentSessionListResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [histories, setHistories] = useState<Record<string, HistoryState>>({});
  const [groupTarget, setGroupTarget] = useState<AgentSessionView | null>(null);
  const [ownerUserID, setOwnerUserID] = useState('');
  const [groupName, setGroupName] = useState('');
  const [groupSubmitting, setGroupSubmitting] = useState(false);
  const [groupError, setGroupError] = useState('');
  const [groupResult, setGroupResult] = useState<CreateAgentSessionGroupResponse | null>(null);
  const requestSerial = useRef(0);
  const currentProject = useRef('');

  useEffect(() => {
    if (!projects.length) {
      setProject('');
      return;
    }
    if (!projects.some((item) => item.name === project)) {
      setProject(projects[0].name);
    }
  }, [project, projects]);

  useEffect(() => {
    currentProject.current = project;
    setExpandedId(null);
    setHistories({});
  }, [project]);

  const loadSessions = useCallback(async () => {
    if (!project) {
      setResult(null);
      setError('');
      return;
    }

    const serial = ++requestSerial.current;
    setLoading(true);
    setError('');
    setResult(null);
    try {
      const response = await listAgentSessions(project, {
        ...rangeParams(timeRange, customSince, customUntil),
        limit: 100,
        scope,
      });
      if (serial === requestSerial.current) {
        setResult(response);
      }
    } catch (requestError) {
      if (serial === requestSerial.current) {
        setResult(null);
        setError(errorMessage(requestError));
      }
    } finally {
      if (serial === requestSerial.current) setLoading(false);
    }
  }, [customSince, customUntil, project, scope, timeRange]);

  useEffect(() => {
    loadSessions();
    return () => {
      requestSerial.current += 1;
    };
  }, [loadSessions]);

  useEffect(() => {
    const handler = () => loadSessions();
    window.addEventListener('cc:refresh', handler);
    return () => window.removeEventListener('cc:refresh', handler);
  }, [loadSessions]);

  const loadHistory = useCallback(async (session: AgentSessionView) => {
    const requestProject = project;
    setHistories((current) => ({
      ...current,
      [session.id]: { loading: true },
    }));
    try {
      const data = await getAgentSessionHistory(requestProject, session.id);
      if (currentProject.current !== requestProject) return;
      setHistories((current) => ({
        ...current,
        [session.id]: { loading: false, data },
      }));
    } catch (historyError) {
      if (currentProject.current !== requestProject) return;
      if (historyError instanceof ApiError && historyError.status === 501) {
        setHistories((current) => ({
          ...current,
          [session.id]: { loading: false, unsupported: true },
        }));
        return;
      }
      setHistories((current) => ({
        ...current,
        [session.id]: { loading: false, error: errorMessage(historyError) },
      }));
    }
  }, [project]);

  const toggleHistory = (session: AgentSessionView) => {
    if (expandedId === session.id) {
      setExpandedId(null);
      return;
    }
    setExpandedId(session.id);
    if (!histories[session.id] || histories[session.id].error) loadHistory(session);
  };

  const handleRowKeyDown = (event: KeyboardEvent<HTMLDivElement>, session: AgentSessionView) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      toggleHistory(session);
    }
  };

  const openGroupModal = (event: MouseEvent, session: AgentSessionView) => {
    event.stopPropagation();
    if (session.bound || !!session.bound_chat_id) return;
    setGroupTarget(session);
    setOwnerUserID('');
    setGroupName('');
    setGroupError('');
    setGroupResult(null);
  };

  const closeGroupModal = () => {
    if (groupSubmitting) return;
    setGroupTarget(null);
  };

  const submitGroup = async () => {
    if (!groupTarget || !ownerUserID.trim()) return;
    setGroupSubmitting(true);
    setGroupError('');
    setGroupResult(null);
    try {
      const response = await createAgentSessionGroup(project, groupTarget.id, {
        owner_user_id: ownerUserID.trim(),
        ...(groupName.trim() ? { name: groupName.trim() } : {}),
      });
      setGroupResult(response);
      await loadSessions();
    } catch (createError) {
      setGroupError(errorMessage(createError));
    } finally {
      setGroupSubmitting(false);
    }
  };

  const sessions = result?.sessions || [];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3 rounded-xl border border-gray-200/80 bg-white/60 p-3 dark:border-white/[0.06] dark:bg-white/[0.03]">
        <label className="space-y-1 text-xs text-gray-500 dark:text-gray-400">
          <span className="block">Project</span>
          <select
            value={project}
            onChange={(event) => setProject(event.target.value)}
            className="min-w-40 rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-sm text-gray-900 focus:outline-none focus:ring-2 focus:ring-accent/50 dark:border-gray-700 dark:bg-gray-800 dark:text-white"
          >
            {projects.map((item) => (
              <option key={item.name} value={item.name}>{item.name}</option>
            ))}
          </select>
        </label>

        <div className="space-y-1">
          <span className="block text-xs text-gray-500 dark:text-gray-400">Scope</span>
          <div className="flex rounded-lg bg-gray-100 p-0.5 dark:bg-white/[0.06]">
            {(['project', 'all'] as const).map((value) => (
              <button
                key={value}
                type="button"
                onClick={() => setScope(value)}
                className={cn(
                  'rounded-md px-3 py-1.5 text-xs transition-colors',
                  scope === value
                    ? 'bg-white text-gray-900 shadow-sm dark:bg-white/[0.12] dark:text-white'
                    : 'text-gray-500 hover:text-gray-800 dark:text-gray-400 dark:hover:text-gray-200',
                )}
              >
                {value === 'project' ? '当前项目' : '全部'}
              </button>
            ))}
          </div>
        </div>

        <div className="space-y-1">
          <span className="block text-xs text-gray-500 dark:text-gray-400">时间范围</span>
          <div className="flex flex-wrap gap-1">
            {([
              ['today', '今天'],
              ['7d', '近 7 天'],
              ['30d', '近 30 天'],
              ['all', '全部'],
              ['custom', '自定义'],
            ] as const).map(([value, label]) => (
              <Button
                key={value}
                type="button"
                size="sm"
                variant={timeRange === value ? 'primary' : 'secondary'}
                onClick={() => setTimeRange(value)}
              >
                {label}
              </Button>
            ))}
          </div>
        </div>

        {timeRange === 'custom' && (
          <div className="flex flex-wrap items-end gap-2">
            <Input
              label="起始时间"
              type="datetime-local"
              value={customSince}
              onChange={(event) => setCustomSince(event.target.value)}
            />
            <Input
              label="结束时间"
              type="datetime-local"
              value={customUntil}
              onChange={(event) => setCustomUntil(event.target.value)}
            />
          </div>
        )}

        <span className="ml-auto text-xs text-gray-400">
          {result?.count ?? sessions.length} sessions
        </span>
      </div>

      {result && result.filtered_unknown_time > 0 && (
        <div className="flex items-center gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-700 dark:border-amber-500/20 dark:bg-amber-900/20 dark:text-amber-300">
          <Clock size={15} />
          {result.filtered_unknown_time} 条会话因缺少时间戳被时间筛选排除
        </div>
      )}

      {result && scope === 'all' && !result.all_sessions_supported && (
        <div className="flex items-center gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-700 dark:border-amber-500/20 dark:bg-amber-900/20 dark:text-amber-300">
          <AlertCircle size={15} />
          该 agent 不支持全量枚举，已回落到当前工作目录
        </div>
      )}

      {error && (
        <div className="flex items-center gap-2 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-500/20 dark:bg-red-900/20 dark:text-red-300">
          <AlertCircle size={15} />
          {error}
        </div>
      )}

      {!project ? (
        <EmptyState message="暂无项目，无法查询原生会话" icon={Bot} />
      ) : loading && !result ? (
        <div className="flex h-48 items-center justify-center animate-pulse text-gray-400">Loading...</div>
      ) : !error && sessions.length === 0 ? (
        <EmptyState message="没有符合筛选条件的 Agent Sessions" icon={MessageSquare} />
      ) : (
        <div className={cn('space-y-3', loading && 'opacity-60')} aria-busy={loading}>
          {sessions.map((session) => {
            const expanded = expandedId === session.id;
            const history = histories[session.id];
            return (
              <div
                key={session.id}
                className="overflow-hidden rounded-xl border border-gray-200/80 bg-white/60 backdrop-blur-sm dark:border-white/[0.06] dark:bg-white/[0.03]"
              >
                <div
                  role="button"
                  tabIndex={0}
                  aria-expanded={expanded}
                  onClick={() => toggleHistory(session)}
                  onKeyDown={(event) => handleRowKeyDown(event, session)}
                  className="cursor-pointer p-4 transition-colors hover:bg-gray-50/80 focus:outline-none focus:ring-2 focus:ring-inset focus:ring-accent/45 dark:hover:bg-white/[0.03]"
                >
                  <div className="flex flex-wrap items-start justify-between gap-3">
                    <div className="min-w-0 flex-1 space-y-2">
                      <div className="flex min-w-0 flex-wrap items-center gap-2">
                        <ChevronDown
                          size={16}
                          className={cn('shrink-0 text-gray-400 transition-transform', expanded && 'rotate-180')}
                        />
                        <span className="max-w-xl truncate text-sm font-medium text-gray-900 dark:text-white">
                          {session.summary || session.id.slice(0, 8)}
                        </span>
                        <Badge variant="info">{session.agent_type}</Badge>
                        <Badge variant={session.bound ? 'success' : 'outline'}>
                          {session.bound ? '已绑定' : '未绑定'}
                        </Badge>
                        {session.time_source === 'unknown' && (
                          <Badge className="text-gray-400">时间未知</Badge>
                        )}
                      </div>
                      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 pl-6 text-xs text-gray-500 dark:text-gray-400">
                        <span>{session.message_count} messages</span>
                        {session.time_source !== 'unknown' && session.modified_at && (
                          <span>{formatTimestamp(session.modified_at)}</span>
                        )}
                        {session.project_path && <span className="break-all">{session.project_path}</span>}
                      </div>
                    </div>

                    <div className="flex shrink-0 flex-col items-end gap-1">
                      <Button
                        type="button"
                        size="sm"
                        variant={session.bound || session.bound_chat_id ? 'secondary' : 'primary'}
                        disabled={!!(session.bound || session.bound_chat_id)}
                        onClick={(event) => openGroupModal(event, session)}
                      >
                        <Users size={14} />
                        {session.bound || session.bound_chat_id ? '已绑定' : '建群'}
                      </Button>
                      {session.bound_chat_id && (
                        <span className="max-w-48 truncate text-[10px] text-gray-400" title={session.bound_chat_id}>
                          {session.bound_chat_id}
                        </span>
                      )}
                    </div>
                  </div>
                </div>

                {expanded && (
                  <div className="border-t border-gray-200/80 bg-gray-50/60 p-4 dark:border-white/[0.06] dark:bg-black/10">
                    <div className="mb-3 flex items-center gap-2 text-xs font-medium text-gray-500 dark:text-gray-400">
                      <History size={14} /> 原生历史
                    </div>
                    {history?.loading ? (
                      <div className="py-6 text-center text-sm text-gray-400 animate-pulse">Loading...</div>
                    ) : history?.unsupported ? (
                      <div className="py-4 text-sm text-gray-500 dark:text-gray-400">该 agent 不支持读取原生历史</div>
                    ) : history?.error ? (
                      <div className="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">
                        {history.error}
                      </div>
                    ) : history?.data && history.data.history.length > 0 ? (
                      <div className="max-h-96 space-y-3 overflow-y-auto pr-1">
                        {history.data.history.map((entry, index) => (
                          <div key={`${entry.timestamp}-${index}`} className="rounded-lg bg-white/80 p-3 dark:bg-white/[0.04]">
                            <div className="mb-1 flex items-center justify-between gap-2">
                              <Badge variant={entry.role === 'user' ? 'info' : 'default'}>{entry.role}</Badge>
                              {entry.timestamp && (
                                <span className="text-[10px] text-gray-400">{formatTimestamp(entry.timestamp)}</span>
                              )}
                            </div>
                            <p className="whitespace-pre-wrap break-words text-sm text-gray-700 dark:text-gray-300">
                              {entry.content}
                            </p>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <div className="py-4 text-sm text-gray-400">暂无历史消息</div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      <Modal open={!!groupTarget} onClose={closeGroupModal} title="为 Agent Session 建群">
        <div className="space-y-4">
          <div className="rounded-lg bg-gray-50 px-3 py-2 text-xs text-gray-500 dark:bg-white/[0.04] dark:text-gray-400">
            {groupTarget?.summary || groupTarget?.id}
          </div>
          <Input
            label="owner_user_id（必填）"
            value={ownerUserID}
            onChange={(event) => setOwnerUserID(event.target.value)}
            placeholder="请输入群主 user ID"
            disabled={groupSubmitting || !!groupResult}
            autoFocus
          />
          <Input
            label="群名（可选）"
            value={groupName}
            onChange={(event) => setGroupName(event.target.value)}
            placeholder="留空则使用默认群名"
            disabled={groupSubmitting || !!groupResult}
          />

          {groupError && (
            <div className="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">
              {groupError}
            </div>
          )}
          {groupResult && (
            <div className="rounded-lg bg-emerald-50 p-3 text-sm text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300">
              <div className="font-medium">{groupResult.created ? '已创建' : '已存在'}</div>
              <div className="mt-1 break-all text-xs">chat_id: {groupResult.chat_id}</div>
            </div>
          )}

          <div className="flex justify-end gap-2">
            <Button type="button" variant="secondary" onClick={closeGroupModal} disabled={groupSubmitting}>
              {groupResult ? '关闭' : '取消'}
            </Button>
            {!groupResult && (
              <Button
                type="button"
                onClick={submitGroup}
                loading={groupSubmitting}
                disabled={!ownerUserID.trim()}
              >
                建群
              </Button>
            )}
          </div>
        </div>
      </Modal>
    </div>
  );
}
