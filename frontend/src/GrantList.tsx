import {useEffect, useState} from 'react';
import type {Grant} from './types';

export function GrantList({grants, revoke}: {grants: Grant[]; revoke: (token: string) => void}) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, []);
  return <section id="v-grants"><h3>Active grants</h3>{!grants.length && <p>No active grants.</p>}{grants.map(grant => {
    const seconds = typeof grant.expires_at === 'number' ? Math.max(0, Math.ceil(grant.expires_at - now / 1000)) : undefined;
    return <div className="v-row" key={grant.token}><span>🎟 {grant.grantee} → {grant.secret}<small> · {grant.scope || 'No origin restriction'} · {seconds === undefined ? 'Expiry unavailable' : seconds === 0 ? 'expired' : `${seconds}s left`}{grant.max_uses ? ` · ${Math.max(0, grant.max_uses - (grant.uses || 0))} of ${grant.max_uses} uses left` : ' · unlimited uses within expiry'}</small></span><button className="v-revoke icon danger" title="Revoke grant" onClick={() => revoke(grant.token)}>✕</button></div>;
  })}</section>;
}
