#!/usr/bin/env python3
"""Publish finalized planning resolutions, then verify every tracked issue.

No product code, provisioning or conformance execution. Refuses to overwrite
unexpected remote edits. Safe to resume after an interrupted GitHub operation.
"""
import argparse,hashlib,json,datetime
import publish_backlog as p
B=p.BASE
RES=json.loads((B/'planning-resolutions.json').read_text())
def sha(body):return hashlib.sha256(body.encode()).hexdigest()
def remote():return {i['number']:i for i in p.pages(f'repos/{p.REPO}/issues?state=all') if 'pull_request' not in i}
def patch(rec,current):
    if current['body']!=rec['body']:
        assert sha(current['body'])==rec['previous_body_sha256'],f"Unexpected remote edit #{rec['number']}"
    payload={k:rec[k] for k in ('body','state','labels')}
    if rec['state']=='closed':payload['state_reason']='completed'
    if all(current[k]==rec[k] for k in ('body','state')) and {l['name'] for l in current['labels']}==set(rec['labels']):return
    p.api(f"repos/{p.REPO}/issues/{rec['number']}",'PATCH',payload)
    print(f"Updated #{rec['number']} {rec['state']}",flush=True)
def verify(complete=True):
    r=remote();expected=set()
    for id,rec in p.state['issues'].items():
        issue=r[rec['number']];expected.add(issue['number'])
        override=RES.get(id)
        if override:
            if not complete and (override['state']=='closed' or id in ('C01','C01-T1')):continue
            assert issue['body']==override['body'],f'body mismatch {id}'
            assert issue['state']==override['state'],f'state mismatch {id}'
            assert {l['name'] for l in issue['labels']}==set(override['labels']),f'labels mismatch {id}'
        else:
            assert sha(issue['body'])==rec['body_sha256'],f'unexpected body change {id}'
            assert issue['state']=='open',f'unexpected completion {id}'
    for s in p.DATA['stories']:
        for item in [s]+s['tasks']:
            m='M00' if item!=s and p.task_planning(s,item) else s['milestone']
            assert r[p.state['issues'][item['id']]['number']]['milestone']['number']==p.state['milestones'][m]['number']
    graph={s['id']:[t['id'] for t in s['tasks']] for s in p.DATA['stories']}
    for s in p.DATA['stories']:
        for i,t in enumerate(s['tasks']):graph[t['id']]=p.task_deps(s,t,i)
    seen=set();active=set()
    def visit(k):
        assert k in graph and k not in active,k
        if k in seen:return
        active.add(k)
        for d in graph[k]:visit(d)
        active.remove(k);seen.add(k)
    for k in graph:visit(k)
    assert len(graph)==220
    for refs in json.loads((B/'coverage.json').read_text()).values():assert set(refs)<=set(graph)
    milestones=p.pages(f'repos/{p.REPO}/milestones?state=all')
    assert len(milestones)==11
    if complete:
        for id in ('1','2'):
            assert r[int(id)]['state']=='closed' and r[int(id)]['body']==RES[id]['body']
        assert sum(i['state']=='closed' for i in r.values())==34
        m=next(m for m in milestones if m['number']==p.state['milestones']['M00']['number'])
        assert m['state']=='closed'
        members=[i for i in r.values() if i.get('milestone') and i['milestone']['number']==m['number']]
        assert len(members)==34 and all(i['state']=='closed' for i in members), 'M00 member state mismatch'
        report={'verified_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'milestones':11,'stories':56,'tasks':164,'format_stories':26,'overview_issues':[1,2],'closed_planning_tasks':30,'closed_planning_stories':2,'closed_overviews':2,'bodies_and_milestones':'matched remote read-back','dependency_graph':'acyclic','scope_coverage':'all functional sections and ten acceptance criteria','implementation':'ready; no application code or test environments built','ready':['C01','C01-T1'],'primary_validation':'OKE + existing MinIO on TrueNAS SCALE','additional_compatibility':'AWS and other S3 providers, reusable suite without environment fleet'}
        (B/'verification.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report,indent=2))
    else:print('Pre-closure remote publication/dependency audit passed',flush=True)
def publish():
    if (B/'implementation-started.json').exists():
        raise RuntimeError('Planning is frozen after implementation started; update active issues individually, never reapply the planning snapshot.')
    r=remote()
    labels={x['name'] for x in p.pages(f'repos/{p.REPO}/labels')}
    if 'status:ready' not in labels:p.api(f'repos/{p.REPO}/labels','POST',{'name':'status:ready','color':'0e8a16','description':'Planning and prerequisites complete; implementation can begin'})
    # Publish evidence first; leave gate and readiness changes to last.
    late={'P01-T2','P01','P02','1','2','C01','C01-T1'}
    for id,rec in RES.items():
        if id not in late:patch(rec,r[rec['number']])
    # Verify completed contract evidence before closing global readiness.
    fresh=remote()
    for id,rec in RES.items():
        if id not in late:assert fresh[rec['number']]['body']==rec['body'] and fresh[rec['number']]['state']==rec['state'],id
    verify(False)
    for id in ('P01-T2','P01','P02','2','1'):
        rec=RES[id];patch(rec,r[rec['number']])
    m=p.state['milestones']['M00']['number']
    current=p.api(f'repos/{p.REPO}/milestones/{m}')
    # Milestone aggregate counters can lag issue state; query actual members.
    assert not p.pages(f'repos/{p.REPO}/issues?milestone={m}&state=open')
    if current['state']!='closed':p.api(f'repos/{p.REPO}/milestones/{m}','PATCH',{'state':'closed'})
    for id in ('C01','C01-T1'):
        rec=RES[id];patch(rec,r[rec['number']])
    verify()
if __name__=='__main__':
    a=argparse.ArgumentParser();a.add_argument('action',choices=['publish','verify']);args=a.parse_args()
    {'publish':publish,'verify':verify}[args.action]()
