#!/usr/bin/env python3
"""Publish the reviewed planning manifest; never executes product implementation.

Uses stable [ID] titles plus a durable map; retries reconcile remotely before creation.
GitHub task links are portable parent/child and dependency references.
"""
import argparse,json,re,subprocess,sys,time
from pathlib import Path
BASE=Path(__file__).resolve().parent
REPO='azrtydxb/Kuvryn-Ant'
ROOT='https://github.com/'+REPO
DATA=json.loads((BASE/'backlog.json').read_text())
MAPFILE=BASE/'github-map.json'
state=json.loads(MAPFILE.read_text()) if MAPFILE.exists() else {'milestones':{},'issues':{}}
last_mutation=0

def api(path,method='GET',payload=None):
    global last_mutation
    if method!='GET':
        time.sleep(max(0,1.05-(time.monotonic()-last_mutation)))
        last_mutation=time.monotonic()
    args=['gh','api',path,'--method',method]
    if payload is not None:args+=['--input','-']
    result=subprocess.run(args,input=json.dumps(payload) if payload is not None else None,text=True,capture_output=True)
    if result.returncode:raise RuntimeError(result.stderr.strip())
    return json.loads(result.stdout) if result.stdout.strip() else None

def pages(path):
    result=[];page=1
    while True:
        chunk=api(path+('&' if '?' in path else '?')+f'per_page=100&page={page}')
        result+=chunk
        if len(chunk)<100:return result
        page+=1

def save():
    tmp=MAPFILE.with_suffix('.tmp');tmp.write_text(json.dumps(state,indent=2)+'\n');tmp.replace(MAPFILE)

def link(id):
    return f"[{id}]({state['issues'][id]['url']})"

def task_planning(s,t):return s['milestone']=='M00' or t.get('planning_only',False)

def story_body(s,with_tasks=False):
    deps=s['depends_on']
    text=f"<!-- kuvryn-plan:{s['id']} -->\n# {s['title']}\n\nAs a **{s['persona']}**, I want **{s['want']}**, so that **{s['benefit']}**.\n\n"
    text+=f"Spec: {s['spec']} ([product specification]({ROOT}/blob/main/kuvryn-ant-product-spec.md)). Milestone: {s['milestone']}.\n\n"
    text+='## Readiness and dependencies\n\n'
    text+=('Blocked by: '+', '.join(link(d) for d in deps)+'.\n' if deps else 'Planning work may proceed. Product implementation remains blocked by the global planning gate.\n')
    text+=f'Planning gate and full delivery plan: {ROOT}/issues/1. No dates or assignees are assumed.\n\n'
    if s['id'].startswith('F'):text+=f'Format capability contract: {ROOT}/issues/2. No ecosystem support is claimed by creating this issue.\n\n'
    text+='## Acceptance criteria and deliverables\n\n'
    for t in s['tasks']:
        text+='### '+t['title']+'\n\n'+'\n'.join('- [ ] '+a for a in t['acceptance'])+'\n\n'
    text+='## Task issues\n\n'
    if with_tasks:text+='\n'.join('- [ ] '+link(t['id'])+' — '+t['title'] for t in s['tasks'])+'\n'
    else:text+='Task issues are being published; this section will be replaced with their links.\n'
    text+='\n## Completion evidence\n\nLink the reviewed contract/design, implementation PRs where applicable, reproducible tests and exact client/backend versions. Include relevant authorization denials, audit, failure/restart/concurrency evidence and operator/client documentation. Mark inapplicable cases with a reason. Closing this story requires all child acceptance checks, not merely merged code.\n\n'
    text+='Out of scope: undocumented protocol compatibility, federation in Phase 1, embedded metadata databases, and selecting a license. Deferred capabilities must be recorded explicitly.\n'
    return text

def task_deps(s,t,index):
    if task_planning(s,t):
        if s['id']=='P01' and index==1:
            return ['P01-T1','P02-T1','P02-T2']+[f'{x["id"]}-T1' for x in DATA['stories'] if x['id'].startswith('F')]
        return []
    deps=['P01-T2']+s['depends_on']
    if index:deps.append(s['tasks'][index-1]['id'])
    deps+=t.get('extra_dependencies',[])
    return list(dict.fromkeys(deps))

def task_body(s,t,index):
    deps=task_deps(s,t,index)
    milestone='M00' if task_planning(s,t) else s['milestone']
    text=f"<!-- kuvryn-plan:{t['id']} -->\n# {t['title']}\n\nParent story: {link(s['id'])}. Spec: {s['spec']}. Milestone: {milestone}.\n\n"
    text+='## Deliverable and acceptance\n\n'+'\n'.join('- [ ] '+a for a in t['acceptance'])+'\n\n'
    text+='## Prerequisites\n\n'
    text+=('Blocked by: '+', '.join(link(d) for d in deps)+'.\n' if deps else 'Planning/research only. May proceed while implementation is paused; do not write product code.\n')
    text+=f'Global planning gate: {ROOT}/issues/1.\n\n'
    text+='## Validation and handoff\n\n'
    if task_planning(s,t):text+='Provide reviewed decisions, source references, testable acceptance cases and an explicit resolved/unresolved register. This task completes only with recorded review; writing a proposal is not approval.\n'
    else:text+='Provide targeted automated/integration or real-client evidence for the criteria above. Exercise relevant denied access, corrupt input, cancellation, concurrent updates and restart paths. Update supported APIs/docs and audit/metrics where the behavior changes; list N/A cases with reasons. Link the implementation and evidence back to the parent story.\n'
    detail_files={'P01-T1':'architecture-baseline.md','P02-T1':'release-workloads.md'}
    if t['id'] in detail_files:
        text+='\n## Review material\n\n'+(BASE/detail_files[t['id']]).read_text()
    return text

def reconcile():
    existing=pages(f'repos/{REPO}/issues?state=all')
    for issue in existing:
        m=re.match(r'^\[([^]]+)\]',issue['title'])
        if m:
            id=m.group(1)
            if id in state['issues'] and state['issues'][id]['number']!=issue['number']:raise RuntimeError('duplicate '+id)
            state['issues'][id]={**state['issues'].get(id,{}),'number':issue['number'],'url':issue['html_url'],'node_id':issue['node_id']}
    save()

def publish():
    reconcile()
    wanted={'type:story':('0075ca','User outcome with explicit acceptance criteria'),'type:task':('1d76db','Bounded deliverable linked to a parent story'),'status:blocked':('b60205','Implementation blocked by planning or prerequisite evidence'),'status:planning':('D4C5F9','Planning or contract research; no product implementation'),'phase:0':('bfdadc','Planning foundation'),'phase:1':('0e8a16','Independent-instance product scope'),'phase:2':('5319e7','Future federation discovery')}
    for d in sorted({s['domain'] for s in DATA['stories']}):wanted['area:'+d]=('c5def5','Workstream: '+d)
    labels={l['name'] for l in pages(f'repos/{REPO}/labels')}
    for name,(color,desc) in wanted.items():
        if name not in labels:api(f'repos/{REPO}/labels','POST',{'name':name,'color':color,'description':desc})
    milestones=pages(f'repos/{REPO}/milestones?state=all')
    for m in DATA['milestones']:
        existing=next((x for x in milestones if x['title']==m['title']),None)
        if not existing:existing=api(f'repos/{REPO}/milestones','POST',{'title':m['title'],'description':m['description']})
        state['milestones'][m['id']]={'number':existing['number'],'url':existing['html_url']};save()
    print('Milestones published',flush=True)
    # Create every story and task before rendering dependency bodies; allows forward references.
    for s in DATA['stories']:
        items=[(s,True)]+[(t,False) for t in s['tasks']]
        for item,is_story in items:
            id=item['id']
            if id in state['issues']:continue
            planning=s['milestone']=='M00' or (not is_story and task_planning(s,item))
            phase='phase:2' if s['milestone']=='M10' else ('phase:0' if planning else 'phase:1')
            labels=['type:story' if is_story else 'type:task','status:planning' if planning else 'status:blocked',phase,'area:'+s['domain']]
            # Initial body contains real acceptance checks even if linking is interrupted.
            checks=[a for t in s['tasks'] for a in t['acceptance']] if is_story else item['acceptance']
            initial=f"<!-- kuvryn-plan:{id} -->\nPlanning publication in progress. Parent/story ID: {s['id']}. Spec: {s['spec']}.\n\n"+'\n'.join('- [ ] '+a for a in checks)+f'\n\nImplementation remains paused. Full plan: {ROOT}/issues/1. Dependency and task links will follow in this same issue.\n'
            created=api(f'repos/{REPO}/issues','POST',{'title':f'[{id}] {item["title"]}','body':initial,'milestone':state['milestones']['M00' if planning else s['milestone']]['number'],'labels':labels})
            state['issues'][id]={'number':created['number'],'url':created['html_url'],'node_id':created['node_id']};save()
            if len(state['issues'])%10==0:print(f"Created/reconciled {len(state['issues'])} issues",flush=True)
    # One body update per issue. Cached content hash makes interrupted runs resumable.
    import hashlib
    for s in DATA['stories']:
        items=[(s,story_body(s,True))]+[(t,task_body(s,t,i)) for i,t in enumerate(s['tasks'])]
        for item,body in items:
            rec=state['issues'][item['id']];digest=hashlib.sha256(body.encode()).hexdigest()
            if rec.get('body_sha256')==digest:continue
            api(f"repos/{REPO}/issues/{rec['number']}",'PATCH',{'body':body})
            rec['body_sha256']=digest;save()
        print('Linked '+s['id'],flush=True)
    render_index()

def render_index():
    text='# GitHub backlog index\n\nSee verification.json for planning completion and implementation readiness. Milestones have no due dates or assignees.\n\n'
    text+=f'Delivery plan: {ROOT}/issues/1. Format ledger: {ROOT}/issues/2.\n\n'
    for m in DATA['milestones']:
        text+=f"## [{m['title']}]({state['milestones'][m['id']]['url']})\n\n"
        for s in DATA['stories']:
            if s['milestone']!=m['id']:continue
            text+=f"### {link(s['id'])} — {s['title']}\n\n"
            text+='\n'.join('- '+link(t['id'])+' — '+t['title'] for t in s['tasks'])+'\n\n'
    (BASE/'github-backlog.md').write_text(text)

def verify():
    remote={i['number']:i for i in pages(f'repos/{REPO}/issues?state=all') if 'pull_request' not in i}
    expected={s['id'] for s in DATA['stories']}|{t['id'] for s in DATA['stories'] for t in s['tasks']}
    assert expected==set(state['issues']), 'unexpected/missing mapped IDs'
    for s in DATA['stories']:
        for item,body in [(s,story_body(s,True))]+[(t,task_body(s,t,i)) for i,t in enumerate(s['tasks'])]:
            rec=state['issues'][item['id']];r=remote[rec['number']]
            assert r['body']==body, 'body mismatch '+item['id']
            planned_milestone='M00' if (item['id']!=s['id'] and task_planning(s,item)) else s['milestone']
            assert r['milestone']['number']==state['milestones'][planned_milestone]['number']
            assert r['state']=='open'
    # Verify task/story graph as it is actually published, including planning-gate references.
    graph={s['id']:[t['id'] for t in s['tasks']] for s in DATA['stories']}
    for s in DATA['stories']:
        for i,t in enumerate(s['tasks']):graph[t['id']]=task_deps(s,t,i)
    seen=set();active=set()
    def visit(id):
        assert id in graph, 'missing reference '+id
        assert id not in active, 'dependency cycle '+id
        if id in seen:return
        active.add(id)
        for d in graph[id]:visit(d)
        active.remove(id);seen.add(id)
    for id in graph:visit(id)
    assert len([s for s in DATA['stories'] if re.fullmatch(r'F\d\d',s['id'])])==26
    print(json.dumps({'verified_milestones':len(state['milestones']),'verified_stories':len(DATA['stories']),'verified_tasks':sum(len(s['tasks']) for s in DATA['stories']),'format_stories':26,'dependency_graph':'acyclic','implementation':'blocked'},indent=2))

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('action',choices=['publish','verify','index']);args=p.parse_args()
    if (BASE/'planning-resolutions.json').exists() and args.action in ('publish','verify'):
        import finalize_planning
        {'publish':finalize_planning.publish,'verify':finalize_planning.verify}[args.action]()
    else:
        {'publish':publish,'verify':verify,'index':render_index}[args.action]()
