import concurrent.futures, http.client, json, time
BASE_PORT=8080
results=[]
def req(path,method='GET',body=None,headers=None):
    c=http.client.HTTPConnection('127.0.0.1',BASE_PORT,timeout=4)
    start=time.monotonic()
    c.request(method,path,body=body,headers=headers or {})
    r=c.getresponse();raw=r.read();elapsed=time.monotonic()-start
    result={'status':r.status,'headers':dict((k.lower(),v) for k,v in r.getheaders()),'raw':raw,'ms':round(elapsed*1000,1)}
    try:result['body']=json.loads(raw)
    except Exception:result['body']=raw.decode(errors='replace')
    c.close();return result

def check(name,fn):
    start=time.monotonic()
    detail=fn()
    results.append({'name':name,'result':'PASS','detail':detail,'ms':round((time.monotonic()-start)*1000,1)})
    print('PASS',name,':',detail,flush=True)

def status(path,want,method='GET',headers=None):
    r=req(path,method,headers=headers);assert r['status']==want,(path,r)
    return f"HTTP {want} in {r['ms']} ms"
check('Health',lambda:status('/health',200))
check('Demo page is served through gateway',lambda:status('/demo/index.html',200))

def forwarding():
    payload=json.dumps({'message':'functional test','items':[1,2,3]})
    r=req('/echo/hello?name=Ary&tag=a&tag=b','POST',payload,{'Content-Type':'application/json','X-Test':'preserved'})
    assert r['status']==200,r
    b=r['body'];assert b['path']=='/hello' and b['query']=='name=Ary&tag=a&tag=b' and b['method']=='POST' and b['body']==payload,b
    assert b['headers']['X-Test']==['preserved'],b
    return 'POST, body, query, custom header preserved; /echo removed'
check('Request forwarding',forwarding)
check('Unknown route',lambda:status('/missing',404))
check('Segment boundary prevents partial route match',lambda:status('/echo-extra',404))

def method():
    r=req('/products','POST');assert r['status']==405 and r['headers']['allow']=='GET',r
    return 'HTTP 405 with Allow: GET'
check('Method filter',method)
check('Missing API key',lambda:status('/private',401))
check('Wrong API key',lambda:status('/private',401,headers={'X-API-Key':'wrong'}))
check('Valid API key',lambda:status('/private',200,headers={'X-API-Key':'demo-key'}))
check('Encoded protected path still requires auth',lambda:status('/%70rivate',401))
check('Ambiguous traversal rejected',lambda:status('/echo/%2e%2e/private',400))
check('Encoded separator rejected',lambda:status('/echo%2fprivate',400))

def identity():
    r=req('/echo/identity',headers={'X-Forwarded-For':'203.0.113.90','Forwarded':'for=203.0.113.90'})
    h=r['body']['headers'];assert h['X-Forwarded-For']==['127.0.0.1'] and 'Forwarded' not in h,h
    return 'Spoofed client IP replaced with actual loopback peer'
check('Forwarded identity cannot be spoofed',identity)

def balance():
    with concurrent.futures.ThreadPoolExecutor(max_workers=20) as pool:
        rr=list(pool.map(lambda _:req('/products/123'),range(80)))
    assert all(r['status']==200 and r['body']['path']=='/123' for r in rr)
    counts={}
    for r in rr:
        name=r['body']['upstream'];counts[name]=counts.get(name,0)+1
    assert counts=={'127.0.0.1:3003':60,'127.0.0.1:3004':20},counts
    return f'80 concurrent requests: {counts}'
check('Weighted balancing under concurrency',balance)

def limit():
    with concurrent.futures.ThreadPoolExecutor(max_workers=20) as pool:
        rr=list(pool.map(lambda _:req('/limited'),range(20)))
    counts={}
    for r in rr:counts[r['status']]=counts.get(r['status'],0)+1
    assert counts=={200:3,429:17},counts
    assert all(int(r['headers']['retry-after'])>=1 for r in rr if r['status']==429)
    return '20 simultaneous requests: 3 accepted, 17 returned 429 with Retry-After'
check('Concurrent rate limit',limit)

def timeout():
    r=req('/timeout/slow?delay=2s');assert r['status']==504 and r['ms']<1000,r
    return f"HTTP 504 in {r['ms']} ms for a 2-second backend delay"
check('Route timeout',timeout)
check('Unavailable backend',lambda:status('/offline',502))
check('Upstream error preserved',lambda:status('/echo/error?status=503',503))
check('Health remains available after failures',lambda:status('/health',200))
print('Waiting for the real 10-second rate-limit window to expire…',flush=True)
time.sleep(10.1)
check('Rate limit recovers after window',lambda:status('/limited',200))

def transformations():
    response = req('/transform/echo', 'POST', json.dumps({'userId': 7, 'userName': 'Ary'}),
                   {'Content-Type': 'application/json', 'X-Debug': 'remove-me'})
    assert response['status'] == 200, response
    received = response['body']['data']
    assert json.loads(received['body']) == {'user': {'id': 7, 'name': 'Ary'}}
    assert 'X-Debug' not in received['headers']
    assert received['headers']['X-Gateway'] == ['gatewaykit']
    assert response['headers']['x-served-by'] == 'gatewaykit'
    assert response['body']['route'] == '/transform'
    return 'Body mapping, header removal/addition, and response envelope verified'
check('Request and response transformations', transformations)

def retry_recovery():
    assert req('/retry-baseline/echo')['status'] == 503
    response = req('/retry/echo')
    assert response['status'] == 200 and response['body']['upstream'] == '127.0.0.1:3006', response
    return '503 baseline; retry succeeds on backend 3006'
check('Retry recovery', retry_recovery)

def health_exclusion():
    responses = [req('/healthy/echo') for _ in range(8)]
    assert all(r['status'] == 200 and r['body']['upstream'] == '127.0.0.1:3006' for r in responses), responses
    return 'All 8 requests avoid the unavailable backend'
check('Active health exclusion', health_exclusion)

def circuit_recovery():
    assert req('/breaker/echo?status=503')['status'] == 503
    assert req('/breaker/echo?status=503')['status'] == 503
    blocked = req('/breaker/echo')
    assert blocked['status'] == 503 and blocked['body']['error'] == 'service_unavailable', blocked
    assert 'x-mock-upstream' not in blocked['headers']
    time.sleep(3.2)
    assert req('/breaker/echo')['status'] == 200
    return 'Two failures open circuit; local rejection; successful recovery after cooldown'
check('Circuit breaker recovery', circuit_recovery)
print(f'All {len(results)} live functional checks passed.', flush=True)
