#!/usr/bin/env python3
"""Private, local PostgreSQL + real app fixture for T3 console QA. No mock API."""
import json, os, pathlib, shutil, signal, socket, subprocess, sys, tempfile, time, urllib.request
state=pathlib.Path(__file__).resolve().parents[1]/'.qa-fixture.json'
repo=pathlib.Path(__file__).resolve().parents[2]
if sys.argv[1:] == ['stop']:
    if state.exists():
        info=json.loads(state.read_text());root=pathlib.Path(info['root'])
        try: os.kill(info['pid'],signal.SIGTERM)
        except ProcessLookupError: pass
        for _ in range(50):
            try: os.kill(info['pid'],0)
            except ProcessLookupError: break
            time.sleep(.1)
        subprocess.run(['pg_ctl','-D',str(root/'pg'),'-m','immediate','stop'],capture_output=True)
        shutil.rmtree(root);state.unlink();print('Fixture stopped and private data removed.')
    sys.exit()
if state.exists():raise SystemExit('Fixture already exists; stop it first.')
os.umask(0o077)
root=pathlib.Path(tempfile.mkdtemp(prefix='ant-console-',dir='/tmp'))
env={k:v for k,v in os.environ.items() if not k.startswith('ANT_')}
env.update(ANT_DATABASE_URL=f'host={root} user=ant dbname=ant_console sslmode=disable',ANT_ROOT_KEY_FILE=str(root/'root.key'),ANT_DATA_DIR=str(root/'storage'),ANT_STORAGE_BACKEND='disk')
(root/'root.key').write_bytes(os.urandom(32))
def run(*args):
    p=subprocess.run(args,env=env,cwd=repo,capture_output=True)
    if p.returncode:
        if args[0]=='go': print(p.stderr.decode(),file=sys.stderr)
        raise RuntimeError(f'{pathlib.Path(args[0]).name} failed; private fixture cleanup follows')
    return p.stdout
server=None
try:
    run('initdb','-D',str(root/'pg'),'-A','trust','-U','ant','--no-locale')
    run('pg_ctl','-D',str(root/'pg'),'-l',str(root/'pg.log'),'-o',f"-k {root} -h ''",'-w','start')
    run('createdb','-h',str(root),'-U','ant','ant_console')
    run('go','build','-o',str(root/'ant'),'./cmd/ant')
    credential=run(str(root/'ant'),'bootstrap','console-qa')
    json.loads(credential)
    (root/'credential.json').write_bytes(credential)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
    env['ANT_LISTEN']=f'127.0.0.1:{port}'
    with (root/'app.log').open('wb') as log:server=subprocess.Popen([str(root/'ant')],env=env,stdout=log,stderr=log,start_new_session=True)
    base=f'http://127.0.0.1:{port}'
    for _ in range(150):
        try:
            urllib.request.urlopen(base+'/health/ready',timeout=1).close();break
        except OSError:
            if server.poll() is not None:raise RuntimeError('App exited; inspect private app log.')
            time.sleep(.1)
    else:raise RuntimeError('App did not become ready.')
    state.write_text(json.dumps({'root':str(root),'pid':server.pid,'base':base}))
    print(json.dumps({'base':base,'credential_file':str(root/'credential.json'),'pid':server.pid}))
except Exception:
    if server:server.terminate()
    subprocess.run(['pg_ctl','-D',str(root/'pg'),'-m','immediate','stop'],capture_output=True)
    shutil.rmtree(root)
    raise
