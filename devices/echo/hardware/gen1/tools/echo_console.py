"""Serial-reader control with acknowledgements; never prints transferred content."""
import argparse, base64, gzip, hashlib, json, os, pathlib, re, time, uuid
ROOT = pathlib.Path(__file__).resolve().parents[3]  # devices/echo
# ECHO_RUNTIME selects another device's reader directory (e.g. local/runtime-echo2).
RUNTIME = pathlib.Path(os.environ.get('ECHO_RUNTIME', ROOT / 'local' / 'runtime'))
def state():
    s=json.loads((RUNTIME/'echo-console-status.json').read_text(encoding='utf-8-sig'))
    if s['phase']!='listening': raise RuntimeError('Console reader is not listening')
    return s
def command(text, timeout=15):
    if '\n' in text or '\r' in text or len(text)>4000: raise ValueError('Expected one bounded line')
    s=state(); log=pathlib.Path(s['log']); offset=log.stat().st_size
    token='ACK_'+uuid.uuid4().hex
    wire=text+"; printf '\\n"+token+":%s\\n' $?"
    payload={'id':uuid.uuid4().hex,'action':'send','text':wire+'\r'}
    p=RUNTIME/'echo-console-control.json'
    # The reader retries partial JSON; the ACK prevents overwriting pending input.
    p.write_text(json.dumps(payload),encoding='utf-8')
    end=time.monotonic()+timeout; received=b''
    while time.monotonic()<end:
        with log.open('rb') as f:
            f.seek(offset); received=f.read()
        match=re.search(rb'(?:\r?\n)'+token.encode()+rb':(\d+)\r?\n',received)
        if match:
            return received[:match.start()].decode('ascii','replace'),int(match.group(1))
        time.sleep(.08)
    raise TimeoutError('No console acknowledgement; inspect private console log')
def upload(source,target,mode='600'):
    data=pathlib.Path(source).read_bytes(); packed=base64.b64encode(gzip.compress(data,compresslevel=9,mtime=0)).decode()
    out,rc=command('stty -echo')
    if rc: raise RuntimeError('Cannot disable terminal echo')
    probe='PRIVATE_PROBE_'+uuid.uuid4().hex
    out,rc=command(': '+probe)
    if probe in out: raise RuntimeError('Terminal echo is still enabled; transfer refused')
    try:
        out,rc=command('umask 077; : > /mnt/echo-net/transfer.b64')
        if rc: raise RuntimeError('Cannot create transfer file')
        for i in range(0,len(packed),2048):
            _,rc=command("printf '%s' '"+packed[i:i+2048]+"' >> /mnt/echo-net/transfer.b64")
            if rc: raise RuntimeError('Transfer append failed')
        py="import binascii;open('/mnt/echo-net/transfer.gz','wb').write(binascii.a2b_base64(open('/mnt/echo-net/transfer.b64').read()))"
        out,rc=command('python -c '+"'"+py.replace("'","'\"'\"'")+"' && gzip -dc /mnt/echo-net/transfer.gz > "+target,timeout=30)
        if rc: raise RuntimeError('Target decompression failed')
        py="import hashlib,os;assert hashlib.sha256(open("+repr(target)+",'rb').read()).hexdigest()=='"+hashlib.sha256(data).hexdigest()+"';os.chmod("+repr(target)+",int('"+mode+"',8));os.unlink('/mnt/echo-net/transfer.b64');os.unlink('/mnt/echo-net/transfer.gz');print('TRANSFER_HASH_OK')"
        out,rc=command('python -c '+"'"+py.replace("'","'\"'\"'")+"'",timeout=30)
        if rc or 'TRANSFER_HASH_OK' not in out: raise RuntimeError('Target hash validation failed')
    finally:
        command('stty echo')
        (RUNTIME/'echo-console-control.json').write_text(json.dumps({'id':uuid.uuid4().hex,'action':'none','text':''}))
    return {'bytes':len(data),'sha256':hashlib.sha256(data).hexdigest(),'verified':True}
if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--command');p.add_argument('--upload');p.add_argument('--target');p.add_argument('--mode',default='600');p.add_argument('--timeout',type=int,default=15);p.add_argument('--private-output')
    a=p.parse_args()
    if a.upload: print(json.dumps(upload(a.upload,a.target,a.mode)))
    else:
        out,rc=command(a.command,a.timeout)
        if a.private_output: pathlib.Path(a.private_output).write_text(out,encoding='utf-8');print('Output saved privately; exit status '+str(rc))
        else: print(out);print('EXIT_STATUS='+str(rc))
        raise SystemExit(rc)
