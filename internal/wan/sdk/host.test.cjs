// Offline release-contract tests. No OAuth, management HTTP, relay WebSocket,
// browser or Bun runtime is used. Run after build.mjs with Node's test runner.
'use strict';
const assert = require('node:assert/strict');
const { test, after } = require('node:test');
const net = require('node:net');
const http = require('node:http');
const https = require('node:https');
const { Duplex, PassThrough } = require('node:stream');
const { once } = require('node:events');

// Every local dial is observed; any accidental non-loopback target fails before
// touching the network. The SDK relay transport below is entirely in memory.
const originalDial = net.createConnection;
const dials = [];
net.createConnection = function (...args) {
    const options = args[0];
    assert.equal(typeof options, 'object', 'a dial must name its fixed loopback target');
    assert.equal(options.host, '127.0.0.1', 'a test may never reach the network');
    dials.push({ ...options });
    return originalDial.apply(this, args);
};
const stdinListeners = process.stdin.listenerCount('data');
const oldConsoleError = console.error;
const { createPortBridge, PortHost, RelayRecovery, sdkFixture: sdk, protocolFixture } = require('./host.cjs');
assert.equal(process.stdin.listenerCount('data'), stdinListeners, 'require must not start IPC/auth/hosting');
assert.equal(console.error, oldConsoleError, 'require must not replace the caller console');
after(() => { net.createConnection = originalDial; });

function deferred() {
    let resolve, reject;
    const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
    return { promise, resolve, reject };
}
function pair() {
    const a = new PassThrough();
    const b = new PassThrough();
    return [Duplex.from({ readable: a, writable: b }), Duplex.from({ readable: b, writable: a })];
}
async function echoServer(t) {
    const sockets = new Set();
    const server = net.createServer(socket => {
        sockets.add(socket);
        socket.on('error', () => {});
        socket.on('close', () => sockets.delete(socket));
        socket.pipe(socket);
    });
    await new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen({ host: '127.0.0.1', port: 0 }, resolve);
    });
    t.after(async () => {
        for (const socket of sockets) socket.destroy();
        await new Promise(resolve => server.close(resolve));
    });
    return server.address().port;
}
function collect(stream, bytes) {
    return new Promise((resolve, reject) => {
        const parts = [];
        let length = 0;
        function data(part) {
            parts.push(part); length += part.length;
            if (length >= bytes) {
                stream.off('data', data);
                stream.off('error', error);
                resolve(Buffer.concat(parts));
            }
        }
        function error(reason) { stream.off('data', data); reject(reason); }
        stream.on('data', data);
        stream.once('error', error);
    });
}

test('strict parent protocol rejects duplicates, invalid UTF-8, and deep objects', () => {
    const parse = text => protocolFixture.parseLine(Buffer.from(text));
    assert.deepEqual(parse('{"a":1,"nested":{"b":true}}'), { a: 1, nested: { b: true } });
    for (const text of ['{"a":1,"a":2}', '{"a":1,"\\u0061":2}', '{"nested":{"a":1,"a":2}}', '{}garbage', '[]', '{"a":NaN}', '{"a":01}', '{"a":"unterminated}']) {
        assert.throws(() => parse(text), undefined, text);
    }
    assert.throws(() => parse('{"a":'.repeat(20) + '0' + '}'.repeat(20)));
    assert.throws(() => protocolFixture.parseLine(Buffer.from([123, 34, 120, 34, 58, 34, 255, 34, 125])));
    assert.equal(protocolFixture.codeFor({ response: { status: 403 } }, 'host'), 'forbidden');
    assert.equal(protocolFixture.codeFor({ response: { status: 401 } }, 'host'), 'auth');
});

test('fixed-port bridge rejects another port, canceled event, and stopped generation with zero dial', async t => {
    const port = await echoServer(t);
    const abort = new AbortController();
    let active = true;
    const bridge = createPortBridge(port, () => active, abort.signal);
    t.after(() => abort.abort());
    const before = dials.length;
    for (const event of [
        { port: port === 65535 ? 1 : port + 1 },
        { port, cancellation: { isCancellationRequested: true } },
    ]) {
        const stream = new PassThrough();
        bridge.handle({ ...event, stream });
        assert.equal(stream.destroyed, true);
    }
    active = false;
    const stopped = new PassThrough();
    bridge.handle({ port, stream: stopped });
    assert.equal(stopped.destroyed, true);
    active = true;
    abort.abort();
    const late = new PassThrough();
    bridge.handle({ port, stream: late });
    assert.equal(late.destroyed, true);
    assert.equal(dials.length, before, 'rejected streams must not even call local dial');
});

test('fixed-port bridge pipes data with backpressure and abort destroys both ends', { timeout: 15000 }, async t => {
    const port = await echoServer(t);
    const abort = new AbortController();
    const tracked = new Set();
    const bridge = createPortBridge(port, () => true, abort.signal, tracked);
    const [remote, local] = pair();
    const peerErrors = [];
    local.on('error', error => peerErrors.push(error));
    const peerClosed = new Promise(resolve => local.once('close', resolve));
    t.after(() => { abort.abort(); remote.destroy(); local.destroy(); });
    bridge.handle({ port, stream: remote, isIncoming: false });
    const received = collect(local, 256 * 1024);
    const payload = Buffer.alloc(256 * 1024, 91);
    local.write(payload);
    assert.deepEqual(await received, payload);
    assert.equal(dials.at(-1).port, port);
    assert.equal(dials.at(-1).family, 4);
    const ends = [...tracked];
    assert.equal(ends.length, 2);
    abort.abort();
    assert.equal(tracked.size, 0);
    for (const stream of ends) assert.equal(stream.destroyed, true);
    await peerClosed;
    assert.equal(local.destroyed, true, 'abort must reach the other in-memory transport end');
    for (const error of peerErrors) assert.equal(error.code, 'ABORT_ERR', 'only the expected Duplexify abort is allowed');
});

async function actualV1(t, providedPort, exercise, recover = false) {
    const port = providedPort || await echoServer(t);
    const extraPort = port === 65535 ? 1 : port + 1;
    const abort = new AbortController();
    let active = true;
    let running = false;
    let deleted = false;
    let metadata = {
        tunnelId: 'offline-contract-fixture', clusterId: 'use',
        accessTokens: { host: 'offline-fake-token-not-a-credential' },
        ports: [{ portNumber: port }],
    };
    const management = {
        updateTunnelEndpoint: async (_tunnel, endpoint) => ({ ...endpoint, hostRelayUri: 'wss://unused.invalid/' }),
        deleteTunnelEndpoints: async () => { deleted = true; return true; },
        getTunnel: async () => metadata,
        reportEvent: () => {},
    };
    const host = new PortHost(management, port, () => active && !abort.signal.aborted);
    const failures = [], progress = [];
    let recoveryClock = 0;
    if (recover) new RelayRecovery(host, () => running, abort.signal, code => failures.push(code),
      () => host.connect(host.tunnel, { enableRetry: false, enableReconnect: false }),
      (fn, ms) => setTimeout(() => { recoveryClock += ms; fn(); }, ms >= 300000 ? ms : Math.min(ms, 10)),
      clearTimeout, () => recoveryClock, value => progress.push(value));
    host.forwardConnectionsToLocalPorts = false;
    const bridge = createPortBridge(port, () => active, abort.signal);
    const firstStream = deferred();
    let events = 0;
    host.forwardedPortConnecting(event => {
        events++;
        firstStream.resolve(event);
        bridge.handle(event);
    });
    const [wireHost, wireRelay] = pair();
    const relay = new sdk.MultiChannelStream(new sdk.NodeStream(wireRelay));
    host.streamFactory = { createRelayStream: async (_uri, protocols) => {
        assert.deepEqual(protocols, [sdk.TunnelRelayTunnelHost.webSocketSubProtocol], 'the actual released SDK must select V1 before module evaluation');
        return { stream: new sdk.NodeStream(wireHost), protocol: sdk.TunnelRelayTunnelHost.webSocketSubProtocol };
    } };
    let client;
    let clientStream;
    t.after(async () => {
        active = false;
        abort.abort();
        clientStream?.destroy();
        client?.dispose();
        relay.dispose();
        wireHost.destroy();
        wireRelay.destroy();
        await host.dispose();
    });
    await Promise.all([relay.connect(), host.connect(metadata, { enableRetry: false, enableReconnect: false })]);
    running = true;
    assert.equal(host.connectionProtocol, sdk.TunnelRelayTunnelHost.webSocketSubProtocol);
    clientStream = await relay.openStream(sdk.TunnelRelayTunnelHost.clientStreamChannelType);
    const configuration = new sdk.SshSessionConfiguration();
    configuration.addService(sdk.PortForwardingService);
    client = new sdk.SshClientSession(configuration);
    client.trace = () => {};
    client.onAuthenticating(event => { event.authenticationPromise = Promise.resolve({}); });
    client.onRequest(event => { event.isAuthorized = ['tcpip-forward', 'cancel-tcpip-forward'].includes(event.request.requestType); });
    const forwarding = client.activateService(sdk.PortForwardingService);
    forwarding.acceptLocalConnectionsForForwardedPorts = false;
    await client.connect(new sdk.NodeStream(clientStream));
    await client.authenticate({ username: 'tunnel' });
    await forwarding.waitForForwardedPort(port);
    const remote = await forwarding.connectToForwardedPort(port);
    t.after(() => remote.destroy());
    const event = await firstStream.promise;
    assert.equal(event.port, port);
    assert.equal(event.isIncoming, false, 'released V1 event does not describe inbound as true');
    assert.equal(event.cancellation, undefined, 'helper lifetime must supply cancellation for V1');
    if (exercise) {
        await exercise(remote, { abort, host, forwarding, port, failures, progress, dropRelay: () => wireHost.destroy() });
    } else {
        const payload = Buffer.from('actual released SDK manual stream');
        const echoed = collect(remote, payload.length);
        remote.write(payload);
        assert.deepEqual(await echoed, payload);
    }
    if (!recover) {
    metadata = { ...metadata, ports: [{ portNumber: port }, { portNumber: extraPort }] };
    await host.refreshPorts();
    assert.equal(forwarding.remoteForwardedPorts.find(value => value.remotePort === extraPort), undefined, 'externally added port must not be advertised');
    const dialsBefore = dials.length;
    await assert.rejects(forwarding.streamToRemotePort('127.0.0.1', extraPort));
    assert.equal(dials.length, dialsBefore, 'a direct request for an external port must not dial');
    }
    assert.equal(events, recover ? 2 : 1);
    active = false;
    abort.abort();
    remote.destroy();
    client.dispose();
    relay.dispose();
    wireHost.destroy();
    wireRelay.destroy();
    await host.dispose();
    assert.equal(deleted, true, 'host.dispose must finish endpoint cleanup');
    assert.equal(host.connectionStatus, sdk.ConnectionStatus.Disconnected);
}

test('actual pinned SDK V1 delivers manual streams, filters refreshed ports, and joins dispose', { timeout: 20000 }, async t => {
    await actualV1(t);
});

test('actual pinned SDK V1 carries SSE and a large slow-consumed HTTP reply, then cancels the active socket', { timeout: 20000 }, async t => {
    const headerWritten = deferred();
    const releaseBody = deferred();
    const serverClosed = deferred();
    const sockets = new Set();
    const payload = Buffer.alloc(512 * 1024, 120);
    const server = http.createServer(async (_request, response) => {
        response.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', 'Content-Length': Buffer.byteLength('data: first\n\n') + payload.length + Buffer.byteLength('\n\ndata: last\n\n') + 100 });
        response.flushHeaders();
        response.write('data: first\n\n');
        headerWritten.resolve();
        await releaseBody.promise;
        if (response.destroyed) return;
        response.write(payload);
        response.write('\n\ndata: last\n\n');
        // Keep this stream open so cancellation, not a normal response end,
        // must close the active server-side socket.
    });
    server.on('connection', socket => {
        sockets.add(socket);
        socket.on('error', () => {});
        socket.on('close', () => { sockets.delete(socket); serverClosed.resolve(); });
    });
    await new Promise(resolve => server.listen({ host: '127.0.0.1', port: 0 }, resolve));
    t.after(async () => {
        releaseBody.resolve();
        for (const socket of sockets) socket.destroy();
        await new Promise(resolve => server.close(resolve));
    });
    await actualV1(t, server.address().port, async (remote, { abort }) => {
        const parts = [];
        const firstReceived = deferred();
        const lastReceived = deferred();
        let paused = false;
        const marker = Buffer.from('\n\ndata: last\n\n');
        remote.on('data', chunk => {
            parts.push(chunk);
            const received = Buffer.concat(parts);
            if (!paused && received.includes(Buffer.from('data: first\n\n'))) {
                paused = true;
                remote.pause();
                firstReceived.resolve();
            }
            if (received.includes(marker)) lastReceived.resolve(received);
        });
        remote.write('GET /events HTTP/1.1\r\nHost: fixture.invalid\r\nConnection: keep-alive\r\n\r\n');
        await headerWritten.promise;
        await firstReceived.promise;
        assert.equal(Buffer.concat(parts).includes(marker), false, 'SSE first event must arrive before the held final body');
        releaseBody.resolve();
        // A state-based slow consumer: the producer is released while the
        // reader is explicitly paused, not after an arbitrary fixed wait.
        assert.equal(remote.isPaused(), true);
        remote.resume();
        const received = await lastReceived.promise;
        assert.equal(received.includes(Buffer.from('text/event-stream')), true);
        assert.equal(received.includes(payload), true, 'the large reply must survive SSH and pipe backpressure');
        abort.abort();
        await serverClosed.promise;
        assert.equal(sockets.size, 0, 'cancel must close the active upstream loopback socket');
    });
});

test('production management options request nested port ACLs at the actual SDK HTTP adapter boundary', { timeout: 10000 }, async t => {
    const requests = [];
    const target = { tunnelId: 'acl-fixture', clusterId: 'use' };
    const port = { portNumber: 43210, protocol: sdk.TunnelProtocol.Http,
        accessControl: { entries: [{ type: 'Anonymous', subjects: [], scopes: [sdk.TunnelAccessScopes.Connect] }] } };
    const adapter = async config => {
        const url = new URL(config.url);
        requests.push({ method: config.method.toUpperCase(), path: url.pathname, query: url.searchParams });
        const data = url.pathname.endsWith('/ports/' + port.portNumber) ? port :
            url.pathname === '/tunnels' ? { value: [{ value: [{ ...target, ports: [port] }] }] } : { ...target, ports: [port] };
        return { status: 200, statusText: 'OK', headers: {}, config, data };
    };
    const management = new sdk.ManagementClient({ name: 'offline-acl-fixture', version: '1' }, sdk.ManagementApiVersions.Version20230927preview,
        async () => 'github offline-fake-token', undefined, undefined, adapter);
    management.enableEventsReporting = false;
    t.after(() => management.dispose());
    // Use the options factory called by prepare, not a test-only copy of flags.
    await management.listTunnels(undefined, undefined, sdk.managementOptions());
    await management.getTunnel(target, sdk.managementOptions());
    await management.getTunnelPort(target, port.portNumber, sdk.managementOptions());
    await management.updateTunnelPort(target, port, sdk.managementOptions());
    assert.deepEqual(requests.map(({ method, path }) => ({ method, path })), [
        { method: 'GET', path: '/tunnels' },
        { method: 'GET', path: '/tunnels/acl-fixture' },
        { method: 'GET', path: '/tunnels/acl-fixture/ports/43210' },
        { method: 'PUT', path: '/tunnels/acl-fixture/ports/43210' },
    ]);
    for (const request of requests) {
        const where = request.method + ' ' + request.path;
        assert.equal(request.query.get('includePorts'), 'true', where + ' must request ports');
        assert.equal(request.query.get('includeAccessControl'), 'true', where + ' must explicitly request access control');
        assert.equal(request.query.get('tokenScopes'), sdk.TunnelAccessScopes.Host, where + ' must retain the host token scope');
        assert.equal(request.query.get('api-version'), sdk.ManagementApiVersions.Version20230927preview);
    }
});

test('actual management and WSS release paths receive the explicit proxy agent without proxy environment fallback', { timeout: 10000 }, async t => {
    const agent = new sdk.HttpsProxyAgent('http://127.0.0.1:1');
    t.after(() => agent.destroy());
    let requestConfig;
    const adapter = async config => {
        requestConfig = config;
        return { status: 200, statusText: 'OK', headers: {}, config, data: { tunnelId: 'proxy-fixture', clusterId: 'use' } };
    };
    const management = new sdk.ManagementClient({ name: 'offline-fixture', version: '1' }, sdk.ManagementApiVersions.Version20230927preview,
        async () => 'github offline-fake-token', undefined, agent, adapter);
    t.after(() => management.dispose());
    await management.getTunnel({ tunnelId: 'proxy-fixture', clusterId: 'use' });
    assert.equal(requestConfig.httpsAgent, agent);
    assert.equal(requestConfig.proxy, false, 'management must not independently choose an environment proxy');
    const originalRequest = https.request;
    let requestOptions;
    https.request = function (options) {
        requestOptions = options;
        throw new Error('offline-wss-request-observed');
    };
    t.after(() => { https.request = originalRequest; });
    const host = new sdk.TunnelRelayTunnelHost(management, () => {});
    host.relayUri = 'wss://offline-fixture.invalid/relay';
    host.accessToken = 'offline-fake-token';
    try {
        await assert.rejects(host.createSessionStream(), /offline-wss-request-observed/);
        assert.equal(requestOptions.agent, agent, 'actual SDK websocket HTTPS options must carry the same agent');
        assert.equal(requestOptions.hostname, 'offline-fixture.invalid');
    } finally {
        https.request = originalRequest;
        await host.dispose();
    }
});

// Error objects can carry credentials in every free-form field. Only fixed
// categories may enter the parent channel.
test('startup failures expose fixed categories without credential material', () => {
    for (const [input, expected] of [
        [{response:{status:400,data:'secret'}}, 'bad_request'],
        [{response:{status:409}}, 'conflict'],
        [{response:{status:429}}, 'rate_limit'],
        [{response:{status:503}}, 'service'],
        [{code:'ENOTFOUND'}, 'network'],
        [{code:'UNABLE_TO_VERIFY_LEAF_SIGNATURE'}, 'tls'],
        [{code:'ECONNABORTED'}, 'timeout'],
        [new TypeError('credential-bearing runtime message'), 'runtime'],
        [{code:'secret',message:'secret'}, 'prepare'],
    ]) assert.equal(protocolFixture.codeFor(input, 'prepare'), expected);
});

test('real service omits false inspection while unsafe port settings stay refused', () => {
    const { validatePortOptions } = require('./host.cjs');
    const port = { protocol: 'http', options: {isHostHeaderUnchanged:true, isOriginHeaderUnchanged:true, requestTimeoutSeconds:0} };
    assert.doesNotThrow(() => validatePortOptions(port, undefined));
    assert.doesNotThrow(() => validatePortOptions({...port,options:{...port.options,isInspectionEnabled:false}}, {isInspectionEnabled:false}));
    for (const value of [true, null, 'false', 0]) {
        assert.throws(() => validatePortOptions({...port,options:{...port.options,isInspectionEnabled:value}}, undefined));
        assert.throws(() => validatePortOptions(port, {isInspectionEnabled:value}));
    }
    for (const options of [{...port.options, requestTimeoutSeconds:undefined}, {...port.options, isHostHeaderUnchanged:false}, {...port.options,isOriginHeaderUnchanged:false}]) {
        assert.throws(() => validatePortOptions({...port, options}, undefined));
    }
    assert.throws(() => validatePortOptions(port, null));
});


test('renewal replaces the real SDK host token only after pinned metadata validates', async t => {
    const { renewHostCredentials } = require('./host.cjs');
    const target = { version: 2, id: 'offline-renewal', clusterId: 'use', port: 12345 };
    const endpoint = 'https://offline-renewal-12345.use.devtunnels.ms';
    const good = () => ({ tunnelId:target.id, clusterId:target.clusterId,
        accessTokens:{host:'new-host-access'}, accessControl:{entries:[]},
        ports:[{portNumber:target.port,protocol:'http',portForwardingUris:[endpoint],
            options:{isHostHeaderUnchanged:true,isOriginHeaderUnchanged:true,requestTimeoutSeconds:0},
            accessControl:{entries:[{type:'Anonymous',subjects:[],scopes:['connect']}]}}] });
    let metadata = good();
    const management = { getTunnel:async (query,options) => {
        assert.deepEqual(query,{tunnelId:target.id,clusterId:target.clusterId});
        assert.equal(options.includeAccessControl,true);
        assert.deepEqual(options.tokenScopes,['host']);
        return metadata;
    }, reportEvent:()=>{}, deleteTunnelEndpoints:async()=>true };
    const host = new PortHost(management,target.port,()=>true);
    t.after(()=>host.dispose());
    const result = await renewHostCredentials(management,host,target,()=>{},endpoint);
    assert.equal(host.tunnel,result);
    assert.equal(host.accessToken,'new-host-access','the pinned SDK setter must receive the new host token');
    const kept = host.tunnel;
    for(const mutate of [
        x=>{x.tunnelId='different-tunnel';}, x=>{x.clusterId='usw2';},
        x=>{x.ports.push({...x.ports[0],portNumber:12346});},
        x=>{x.ports[0].options.isInspectionEnabled=true;},
        x=>{x.ports[0].accessControl.entries[0].scopes=['host'];},
        x=>{x.accessTokens={};}, x=>{x.ports[0].portForwardingUris=['https://different-12345.use.devtunnels.ms'];},
    ]) {
        metadata=good();mutate(metadata);
        await assert.rejects(()=>renewHostCredentials(management,host,target,()=>{},endpoint));
        assert.equal(host.tunnel,kept,'a refused refresh cannot replace the live host');
    }
    metadata=good();
    await assert.rejects(()=>renewHostCredentials(management,host,target,()=>{throw Error('stopped')},endpoint),/stopped/);
    assert.equal(host.tunnel,kept);
});


test('actual pinned SDK V1 keeps the same stream connected through host credential renewal', { timeout: 20000 }, async t => {
    const { renewHostCredentials } = require('./host.cjs');
    await actualV1(t, undefined, async (remote, { host, port }) => {
        const echo = async text => {
            const bytes=Buffer.from(text), received=collect(remote,bytes.length);
            remote.write(bytes);assert.deepEqual(await received,bytes);
        };
        await echo('before renewal');
        const endpoint='https://offline-contract-fixture-'+port+'.use.devtunnels.ms';
        const current={tunnelId:'offline-contract-fixture',clusterId:'use',accessTokens:{host:'renewed-host-access'},accessControl:{entries:[]},
            ports:[{portNumber:port,protocol:'http',portForwardingUris:[endpoint],options:{isHostHeaderUnchanged:true,isOriginHeaderUnchanged:true,requestTimeoutSeconds:0},accessControl:{entries:[{type:'Anonymous',subjects:[],scopes:['connect']}]}}]};
        await renewHostCredentials({getTunnel:async()=>current},host,{version:2,id:current.tunnelId,clusterId:'use',port},()=>{},endpoint);
        assert.equal(host.accessToken,'renewed-host-access');
        assert.equal(host.connectionStatus,sdk.ConnectionStatus.Connected);
        await echo('after renewal, on the same forwarded stream');
    });
});
test('actual management adapter sends the provider-specific service authorization without borrowing another identity', async () => {
  for (const provider of ['github', 'microsoft']) {
    let observed;
    const management = new sdk.ManagementClient({ name: 'offline-identity-fixture', version: '1' }, sdk.ManagementApiVersions.Version20230927preview,
      async () => protocolFixture.serviceAuthorization(provider, 'fixture-credential'), undefined, undefined,
      async config => { observed = config.headers.Authorization; return { data: [], status: 200, statusText: 'OK', headers: {}, config }; });
    management.enableEventsReporting = false;
    await management.listUserLimits();
    assert.equal(observed, (provider === 'microsoft' ? 'Bearer ' : 'github ') + 'fixture-credential');
    await management.dispose();
  }
  assert.throws(() => protocolFixture.serviceAuthorization('other', 'fixture-credential'));
  assert.throws(() => protocolFixture.serviceAuthorization('microsoft', 'bad\r\ncredential'));
});


function relayRecoveryFixture() {
    const statuses=new Set(), retries=new Set(), timers=new Map(), failures=[], attempts=[], progress=[];
    let next=0, running=false, now=0, succeeds=false, held;
    const abort=new AbortController();
    const relay={disconnectReason:sdk.SshDisconnectReason.connectionLost,
      connectionStatusChanged:fn=>{statuses.add(fn);return{dispose:()=>statuses.delete(fn)}},
      retryingTunnelConnection:fn=>{retries.add(fn);return{dispose:()=>retries.delete(fn)}}};
    const status=value=>{for(const fn of [...statuses])fn({status:value})};
    const policy=new RelayRecovery(relay,()=>running,abort.signal,code=>failures.push(code),async()=>{
      attempts.push(now);
      if(held)return held.promise;
      if(succeeds){status(sdk.ConnectionStatus.Connected);return}
      throw Object.assign(new Error('fixture transport failure'),{code:'ECONNRESET'});
    },(fn,ms)=>{const id=next++;timers.set(id,{fn,at:now+ms});return id},id=>timers.delete(id),()=>now,value=>progress.push(value));
    return {relay,policy,abort,timers,failures,attempts,progress,status,start:()=>{running=true},succeed:()=>{succeeds=true},
      hold:()=>{held=deferred();return held},lose:()=>status(sdk.ConnectionStatus.Disconnected),
      retry:error=>{const e={error,retry:true,delayMs:20000};for(const fn of [...retries])fn(e);return e},
      advance:async target=>{for(;;){const entry=[...timers].filter(([,t])=>t.at<=target).sort((a,b)=>a[1].at-b[1].at||a[0]-b[0])[0];if(!entry)break;now=entry[1].at;timers.delete(entry[0]);entry[1].fn();for(let i=0;i<12;i++)await Promise.resolve()}now=target}};
}

test('relay recovery tries three times per minute for five minutes and reports exhaustion exactly once',async()=>{
  const f=relayRecoveryFixture();
  f.status(sdk.ConnectionStatus.Connecting);
  assert.equal(f.timers.size,0);assert.equal(f.retry({code:'ECONNRESET'}).retry,false,'SDK internal retries must stay off');
  f.start();f.lose();f.status(sdk.ConnectionStatus.Connecting);f.status(sdk.ConnectionStatus.RefreshingTunnelAccessToken);
  await f.advance(299999);
  assert.deepEqual(f.attempts,Array.from({length:15},(_,i)=>i*20000));
  assert.deepEqual(f.failures,[],'the user must not be told five minutes failed before the full interval');
  await f.advance(300000);assert.deepEqual(f.failures,['reconnect_timeout']);assert.equal(f.timers.size,0);
  f.status(sdk.ConnectionStatus.Connected);await f.advance(600000);
  assert.equal(f.attempts.length,15);assert.deepEqual(f.failures,['reconnect_timeout']);
});

test('successful relay recovery clears the budget, while Stop joins an in-flight attempt and prevents later retries',async()=>{
  const f=relayRecoveryFixture();f.start();f.lose();await f.advance(40000);f.succeed();await f.advance(60000);
  assert.deepEqual(f.attempts,[0,20000,40000,60000]);assert.equal(f.timers.size,0);assert.deepEqual(f.failures,[]);
  f.lose();await f.advance(60000);assert.equal(f.attempts.length,5);assert.equal(f.timers.size,0);
  f.policy.dispose();
  const stopped=relayRecoveryFixture();stopped.start();const held=stopped.hold();stopped.lose();await stopped.advance(0);
  assert.equal(stopped.attempts.length,1);stopped.abort.abort();held.reject(Object.assign(new Error('canceled transport'),{code:'ECONNRESET'}));
  await stopped.policy.wait();await stopped.advance(600000);
  assert.equal(stopped.timers.size,0);assert.equal(stopped.attempts.length,1);assert.deepEqual(stopped.failures,[]);
});

test('relay recovery refuses authentication, policy, TLS, host takeover and rate-limit failures',()=>{
  for(const error of [{response:{status:401}},{errorContext:{statusCode:403}},{code:'CERT_HAS_EXPIRED'},{response:{status:429}}]){
    const f=relayRecoveryFixture();f.start();f.relay.disconnectError=error;f.lose();
    assert.equal(f.timers.size,0);assert.equal(f.failures.length,1);
  }
  const initial=relayRecoveryFixture();initial.lose();assert.equal(initial.timers.size,0);assert.equal(initial.failures.length,1);
  const conflict=relayRecoveryFixture();conflict.start();conflict.relay.disconnectReason=sdk.SshDisconnectReason.tooManyConnections;conflict.lose();
  assert.deepEqual(conflict.failures,['host_conflict']);assert.equal(conflict.timers.size,0);
});

test('actual pinned SDK V1 reconnects the same tunnel after transport loss and one failed network retry',{timeout:20000},async t=>{
  await actualV1(t,undefined,async(remote,{host,port,failures,progress,dropRelay})=>{
    remote.on('error',()=>{});
    const [newHost,newRelay]=pair();
    const relay=new sdk.MultiChannelStream(new sdk.NodeStream(newRelay));
    let attempts=0;
    host.streamFactory={createRelayStream:async()=>{
      attempts++;
      if(attempts===1)throw new sdk.SshConnectionError('simulated relay transport reset',sdk.SshDisconnectReason.connectionLost);
      return{stream:new sdk.NodeStream(newHost),protocol:sdk.TunnelRelayTunnelHost.webSocketSubProtocol};
    }};
    let session,stream;
    t.after(()=>{stream?.destroy();session?.dispose();relay.dispose();newHost.destroy();newRelay.destroy()});
    const connected=new Promise(resolve=>{
      const sub=host.connectionStatusChanged(e=>{if(e.status===sdk.ConnectionStatus.Connected){sub.dispose();resolve()}});
      t.after(()=>sub.dispose());
    });
    const relayConnected=relay.connect();
    await host.sshSession.close(sdk.SshDisconnectReason.connectionLost, 'simulated relay transport loss');
    await Promise.all([relayConnected,connected]);
    assert.equal(attempts,2);assert.deepEqual(failures,[]);assert.deepEqual(progress.filter(p=>p?.retrying).map(p=>p.attempts),[1,2]);assert.equal(progress.at(-1),null);assert.equal(host.tunnel.tunnelId,'offline-contract-fixture');
    stream=await relay.openStream(sdk.TunnelRelayTunnelHost.clientStreamChannelType);
    const cfg=new sdk.SshSessionConfiguration();cfg.addService(sdk.PortForwardingService);
    session=new sdk.SshClientSession(cfg);session.trace=()=>{};
    session.onAuthenticating(e=>{e.authenticationPromise=Promise.resolve({})});
    session.onRequest(e=>{e.isAuthorized=['tcpip-forward','cancel-tcpip-forward'].includes(e.request.requestType)});
    const forwarding=session.activateService(sdk.PortForwardingService);forwarding.acceptLocalConnectionsForForwardedPorts=false;
    await session.connect(new sdk.NodeStream(stream));await session.authenticate({username:'tunnel'});
    await forwarding.waitForForwardedPort(port);
    const forwarded=await forwarding.connectToForwardedPort(port);forwarded.on('error',()=>{});t.after(()=>forwarded.destroy());
    const data=Buffer.from('new request after relay recovery'),received=collect(forwarded,data.length);
    forwarded.write(data);assert.deepEqual(await received,data);
  },true);
});


test('relay recovery reports actual attempts, waiting cadence and successful recovery',async()=>{
  const f=relayRecoveryFixture();f.start();f.lose();
  assert.deepEqual(f.progress,[{attempts:0,retryInMs:0,retrying:false}]);
  await f.advance(0);
  assert.deepEqual(f.progress.slice(-2),[{attempts:1,retryInMs:0,retrying:true},{attempts:1,retryInMs:20000,retrying:false}]);
  await f.advance(19999);assert.equal(f.attempts.length,1);
  f.succeed();await f.advance(20000);
  assert.deepEqual(f.progress.slice(-2),[{attempts:2,retryInMs:0,retrying:true},null]);
  assert.equal(f.timers.size,0);
  f.policy.dispose();assert.equal(f.progress.at(-1),null);
});
