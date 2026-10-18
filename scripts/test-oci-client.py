#!/usr/bin/env python3
"""Local real-client smoke test; no container/cloud/provider environment required.
Requires installed PostgreSQL binaries, Go and crane. All secrets/fixtures are
private temporary files; prints only client version and validation results.
"""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tarfile
import tempfile
import time
import urllib.request
import urllib.error


def main():
    for tool in ('initdb', 'pg_ctl', 'createdb', 'go', 'crane'):
        if not shutil.which(tool):
            raise SystemExit(f'required tool missing: {tool}')
    with tempfile.TemporaryDirectory(prefix='ant-oci-') as directory:
        root = Path(directory)
        env = os.environ.copy()
        env.update(ANT_DATABASE_URL=f'host={root} user=ant dbname=ant_test sslmode=disable',
                   ANT_ROOT_KEY_FILE=str(root/'root.key'), ANT_DATA_DIR=str(root/'storage'),
                   ANT_STORAGE_BACKEND='disk', DOCKER_CONFIG=str(root/'docker'))
        env.pop('ANT_INSTANCE_ID', None)
        (root/'root.key').write_bytes(os.urandom(32))
        (root/'root.key').chmod(0o600)
        def run(*args, data=None, environment=None, expect_failure=False):
            result = subprocess.run(args, input=data, env=environment or env, capture_output=True)
            if expect_failure:
                if result.returncode == 0:
                    raise RuntimeError(f'{Path(args[0]).name} unexpectedly succeeded')
                return result.stdout
            if result.returncode:
                # Child tools may print credentials: never echo raw output here.
                raise RuntimeError(f'{Path(args[0]).name} failed with exit {result.returncode}')
            return result.stdout
        run('initdb', '-D', str(root/'pg'), '-A', 'trust', '-U', 'ant', '--no-locale')
        server = None
        try:
            run('pg_ctl', '-D', str(root/'pg'), '-l', str(root/'pg.log'), '-o', f"-k {root} -h ''", '-w', 'start')
            run('createdb', '-h', str(root), '-U', 'ant', 'ant_test')
            run('go', 'build', '-o', str(root/'ant'), './cmd/ant')
            credential = json.loads(run(str(root/'ant'), 'bootstrap', 'oci-client-test'))
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            env['ANT_LISTEN'] = f'127.0.0.1:{port}'
            log = (root/'service.log').open('wb')
            server = subprocess.Popen([str(root/'ant')], env=env, stdout=log, stderr=log)
            base = f'http://127.0.0.1:{port}'
            def request(method, path, body=None, revision=None):
                headers = {'Authorization': 'Bearer '+credential['token'], 'Content-Type': 'application/json', 'Idempotency-Key': hashlib.sha256((method+path+json.dumps(body)).encode()).hexdigest()}
                if revision is not None:
                    headers['If-Match'] = f'"{revision}"'
                req = urllib.request.Request(base+path, None if body is None else json.dumps(body).encode(), headers, method=method)
                with urllib.request.urlopen(req, timeout=10) as response:
                    payload = response.read()
                    return json.loads(payload) if payload else None
            def await_ready():
                deadline = time.monotonic()+15
                while True:
                    try:
                        request('GET', '/health/ready')
                        return
                    except OSError:
                        if server.poll() is not None or time.monotonic()>deadline:
                            raise RuntimeError('service startup failed')
                        time.sleep(.1)
            def registry_request(method, path, data=b'', headers=None):
                values = {'Authorization':'Bearer '+credential['token'],
                          'Content-Type':'application/octet-stream'}
                values.update(headers or {})
                req = urllib.request.Request(base+path, data if method in ('POST','PATCH','PUT') else None,
                                             values, method=method)
                with urllib.request.urlopen(req, timeout=10) as response:
                    return response.status, response.headers, response.read()
            await_ready()
            feed = request('POST', '/api/v1/feeds', {'slug':'images', 'format':'oci'})
            source = request('POST', '/api/v1/sources', {'name':'local-images', 'kind':'local', 'format':'oci'})
            request('PUT', f"/api/v1/feeds/{feed['id']}/sources/{source['id']}", {'priority':0}, feed['revision'])
            feed = request('GET', '/api/v1/feeds/'+feed['id'])
            request('PUT', f"/api/v1/feeds/{feed['id']}/publish-policy", {'default_source_id':source['id'], 'rules':[]}, feed['revision'])
            registry = f'127.0.0.1:{port}'
            run('crane', 'auth', 'login', registry, '-u', 'service', '--password-stdin', data=credential['token'].encode())
            layer = root/'layer.tar'
            with tarfile.open(layer, 'w') as archive:
                data=b'Kuvryn Ant real-client fixture\n'
                entry=tarfile.TarInfo('fixture.txt');entry.size=len(data);entry.mode=0o644
                archive.addfile(entry, io.BytesIO(data))
            reference=registry+'/images/demo:smoke'
            run('crane','append','--oci-empty-base','-f',str(layer),'-t',reference,'--insecure')
            digest=run('crane','digest',reference,'--insecure').decode().strip()
            manifest=run('crane','manifest',reference,'--insecure')
            if digest!='sha256:'+hashlib.sha256(manifest).hexdigest():
                # crane manifest may append a newline for terminal output.
                if digest!='sha256:'+hashlib.sha256(manifest.rstrip(b'\n')).hexdigest():
                    raise RuntimeError('manifest digest mismatch')
            run('crane','pull',reference,str(root/'image.tar'),'--insecure')
            with tarfile.open(root/'image.tar') as archive:
                if not archive.getmembers(): raise RuntimeError('empty pulled image')
            # Leave a partially committed upload, restart the complete process,
            # then complete the session and read it with the real registry client.
            resumable=b'persisted OCI upload across process restart'
            status, headers, _ = registry_request('POST','/v2/images/demo/blobs/uploads/')
            if status != 202: raise RuntimeError('upload initialization failed')
            location=headers['Location']
            status, _, _ = registry_request('PATCH',location,resumable[:11],{'Content-Range':'0-10'})
            if status != 202: raise RuntimeError('upload checkpoint failed')
            server.send_signal(signal.SIGTERM)
            server.wait(timeout=20)
            server = subprocess.Popen([str(root/'ant')],env=env,stdout=log,stderr=log)
            await_ready()
            status, headers, _ = registry_request('GET',location)
            if status != 204 or headers.get('Range') != '0-10':
                raise RuntimeError('upload checkpoint did not survive restart')
            resumed_digest='sha256:'+hashlib.sha256(resumable).hexdigest()
            status, _, _ = registry_request('PUT',location+'?digest='+resumed_digest,resumable[11:],
                                            {'Content-Range':f'11-{len(resumable)-1}'})
            if status != 201: raise RuntimeError('resumed upload did not finalize')
            if run('crane','blob',registry+'/images/demo@'+resumed_digest,'--insecure') != resumable:
                raise RuntimeError('resumed blob bytes changed')
            if run('crane','digest',reference,'--insecure').decode().strip() != digest:
                raise RuntimeError('published manifest did not survive restart')
            run('crane','pull',reference,str(root/'after-restart.tar'),'--insecure')

            amd64=registry+'/images/demo:amd64'
            arm64=registry+'/images/demo:arm64'
            multi=registry+'/images/demo:multi'
            run('crane','mutate',reference,'--set-platform','linux/amd64','-t',amd64,'--insecure')
            run('crane','mutate',reference,'--set-platform','linux/arm64','-t',arm64,'--insecure')
            run('crane','index','append','-m',amd64,'-m',arm64,'-t',multi,'--insecure')
            index=json.loads(run('crane','manifest',multi,'--insecure'))
            platforms={(entry.get('platform',{}).get('os'),entry.get('platform',{}).get('architecture'))
                       for entry in index['manifests']}
            if platforms != {('linux','amd64'),('linux','arm64')}:
                raise RuntimeError('multi-platform index descriptors differ')
            run('crane','pull',multi,str(root/'arm64.tar'),'--platform','linux/arm64','--insecure')
            with tarfile.open(root/'arm64.tar') as archive:
                catalog=json.load(archive.extractfile('manifest.json'))
                config=json.load(archive.extractfile(catalog[0]['Config']))
                if config.get('architecture') != 'arm64' or config.get('os') != 'linux':
                    raise RuntimeError('platform-selected pull used wrong image')

            # Separate disposable client credentials prove a rejected token does
            # not fall back to the valid login. No tool output containing auth is printed.
            bad_config=root/'bad-docker'
            bad_config.mkdir(mode=0o700)
            config_file=bad_config/'config.json'
            config_file.write_text(json.dumps({'auths':{registry:{'auth':base64.b64encode(b'service:invalid-token').decode()}}}))
            config_file.chmod(0o600)
            bad_env=env.copy();bad_env['DOCKER_CONFIG']=str(bad_config)
            run('crane','digest',reference,'--insecure',environment=bad_env,expect_failure=True)
            for token in (None,'invalid-token'):
                headers={} if token is None else {'Authorization':'Bearer '+token}
                try:
                    urllib.request.urlopen(urllib.request.Request(base+'/v2/',headers=headers),timeout=10)
                    raise RuntimeError('registry accepted missing/invalid credentials')
                except urllib.error.HTTPError as error:
                    if error.code != 401: raise RuntimeError('unexpected registry auth rejection')
                    body=json.loads(error.read())
                    if body['errors'][0]['code'] != 'UNAUTHORIZED':
                        raise RuntimeError('registry auth rejection had wrong envelope')

            print('Client:',run('crane','version').decode().strip())
            print('PASS: API setup, authenticated push/pull, durable upload restart, multi-platform index and rejected credentials')
        finally:
            if server is not None:
                server.send_signal(signal.SIGTERM)
                try: server.wait(timeout=20)
                except subprocess.TimeoutExpired: server.kill();server.wait()
            subprocess.run(['pg_ctl','-D',str(root/'pg'),'-m','immediate','stop'],capture_output=True)

if __name__=='__main__':
    main()
