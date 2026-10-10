// Pending agent actions, as the owner reads them. Pure functions only: the
// panel renders what these return, and the tests pin the wording.
//
// Everything here is provider- or agent-written text (titles, bodies, recipients).
// Callers render it as text, never as HTML.

export type ActionState = 'pending' | 'running' | 'executed' | 'failed' | 'denied' | (string & {});

export interface SourceAction {
  id: string;
  source: string;
  kind?: string;
  action: string;
  params?: Record<string, string>;
  summary?: string;
  agent?: string;
  state: ActionState;
  created: string;
  decided?: string;
  decided_by?: string;
  note?: string;
  result?: { id?: string; url?: string; message?: string };
  error?: string;
}

export interface ConnectorName { id: string; name: string; kind: string; config?: Record<string, string> }

// The order the owner reads parameters in; anything else follows alphabetically.
const PARAM_ORDER = ['to', 'cc', 'subject', 'body', 'channel', 'text', 'thread_ts', 'repo', 'title', 'labels',
  'number', 'summary', 'start', 'end', 'attendees', 'location', 'notify', 'description', 'folder_id'];

const KIND_LABEL: Record<string, string> = {
  gmail: 'Gmail', outlook: 'Outlook', gcal: 'Google Calendar', gdrive: 'Google Drive',
  slack: 'Slack', github: 'GitHub', imap: 'IMAP', onedrive: 'OneDrive', linear: 'Linear',
  discord: 'Discord', notion: 'Notion', readwise: 'Readwise', rss: 'RSS', confluence: 'Confluence', jira: 'Jira',
};

export const kindLabel = (kind?: string): string => (kind && KIND_LABEL[kind]) || kind || 'Connector';

export function clip(text: string | undefined, max = 120): string {
  const flat = (text ?? '').replace(/\s+/g, ' ').trim();
  if (!flat) return '(empty)';
  return [...flat].length > max ? [...flat].slice(0, max).join('') + '…' : flat;
}

const quoted = (text: string | undefined) => `"${clip(text, 100)}"`;

// The headline: what the action does, in plain words. The full parameters are
// shown separately on expand.
export function plainAction(action: SourceAction, connector?: ConnectorName): string {
  const p = action.params ?? {};
  const key = `${action.kind ?? connector?.kind ?? ''}.${action.action}`;
  // GitHub actions are pinned to the connector's repository, so that is the name to show.
  const repo = p.repo || connector?.config?.repo || connector?.name || 'the connected repository';
  switch (key) {
    case 'gmail.create_draft':
    case 'outlook.create_draft':
      return `Draft an email to ${clip(p.to, 80)}: subject ${quoted(p.subject)}`;
    case 'gmail.send_message':
      return `Send an email to ${clip(p.to, 80)}: subject ${quoted(p.subject)}`;
    case 'slack.post_message':
      return `Post to channel ${clip(p.channel, 40)}: ${clip(p.text)}`;
    case 'github.create_issue':
      return `Create GitHub issue in ${repo}: ${clip(p.title)}`;
    case 'github.comment':
      return `Comment on GitHub #${clip(p.number, 12)} in ${repo}: ${clip(p.body)}`;
    case 'gcal.create_event': {
      const when = p.start ? ` from ${clip(p.start, 40)}${p.end ? ` to ${clip(p.end, 40)}` : ''}` : '';
      return `Create calendar event ${quoted(p.summary)}${when}`;
    }
    case 'gdrive.create_doc':
      return `Create Google Doc ${quoted(p.title)}`;
    default:
      return action.summary?.trim() || `${kindLabel(action.kind)}: ${action.action}`;
  }
}

// Parameters as plain text, one per line. Values are raw: a body may contain
// markup, and it must read as the text it is.
export function paramLines(params?: Record<string, string>): string[] {
  const entries = Object.entries(params ?? {});
  const rank = (name: string) => {
    const i = PARAM_ORDER.indexOf(name);
    return i < 0 ? PARAM_ORDER.length : i;
  };
  entries.sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b));
  return entries.map(([name, value]) => `${name}: ${value ?? ''}`);
}

export const isPending = (a: SourceAction) => a.state === 'pending';
export const isDecided = (a: SourceAction) => a.state === 'executed' || a.state === 'failed' || a.state === 'denied';

export const pendingCount = (actions: SourceAction[]) => actions.filter(isPending).length;

export function stateLabel(state: ActionState): string {
  switch (state) {
    case 'pending': return 'Waiting';
    case 'running': return 'Running';
    case 'executed': return 'Done';
    case 'failed': return 'Failed';
    case 'denied': return 'Denied';
    default: return state;
  }
}

// What happened, in one line: the provider's confirmation, the error, or the note.
export function outcomeText(action: SourceAction): string {
  switch (action.state) {
    case 'executed': return action.result?.message || 'Done.';
    case 'failed': return `Failed: ${action.error || 'no reason was recorded'}`;
    case 'denied': return action.note ? `Denied: ${action.note}` : 'Denied.';
    case 'running': return 'Running now.';
    default: return 'Waiting for your decision.';
  }
}

// Only web links from provider results are offered as links.
export function safeLink(url?: string): string | undefined {
  return url && /^https?:\/\//i.test(url) ? url : undefined;
}

// Newest first, as the server returns them, but never trust that order for the
// decided history: sort by decision time, falling back to creation.
export function sortHistory(actions: SourceAction[]): SourceAction[] {
  return actions.filter(isDecided).sort((a, b) => (b.decided || b.created).localeCompare(a.decided || a.created));
}

export function whenText(stamp?: string, now = new Date()): string {
  if (!stamp) return '';
  const when = new Date(stamp);
  if (Number.isNaN(when.getTime())) return stamp;
  const sameDay = when.toDateString() === now.toDateString();
  return sameDay
    ? when.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
    : when.toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}
