// Copyright (c) magpie contributors.
// The official SDK owns the relay/SSH protocol. This helper owns only a fixed
// port's stream bridge and the private, generation-bound parent protocol.
import type { Tunnel, TunnelPort } from '@microsoft/dev-tunnels-contracts';
import type { ForwardedPortConnectingEventArgs, PortForwardingService } from '@microsoft/dev-tunnels-ssh-tcp';
import type { Duplex } from 'stream';
import type { Socket } from 'net';

// 1.3.6 exposes manual forwardedPortConnecting streams for the V1 host path.
// Its V2 path does not raise that event when automatic local forwarding is off.
// This documented SDK environment switch is read at module initialization, so
// there must be no value-import of connections above this assignment.
process.env.DEVTUNNELS_PROTOCOL_VERSION = '1';
delete process.env.DEBUG;
const { TunnelRelayTunnelHost, ConnectionStatus } = require('@microsoft/dev-tunnels-connections');
const { TunnelManagementHttpClient, ManagementApiVersions } = require('@microsoft/dev-tunnels-management');
const { TunnelAccessScopes, TunnelAccessControlEntryType, TunnelProtocol } = require('@microsoft/dev-tunnels-contracts');
const { CancellationTokenSource } = require('vscode-jsonrpc');
const { HttpsProxyAgent } = require('https-proxy-agent');
const { createConnection } = require('net');
const { randomBytes } = require('crypto');
const { TextDecoder } = require('util');

// Neither diagnostics nor upstream Error objects belong on either output pipe.
if (require.main === module) {
    for (const name of ['log', 'info', 'warn', 'error', 'debug', 'trace'] as const) {
        console[name] = () => {};
    }
}

const wireVersion = 1;
const lineLimit = 64 * 1024;
const serviceClientID = 'Iv1.e7b89e013f801f03';
const microsoftClientID = 'c0df98ca-23b4-4bce-bb9f-72039b28d3a5';
const idPattern = /^[a-z0-9][a-z0-9-]{1,58}[a-z0-9]$/;
const clusterPattern = /^(([a-z]{3,4}[0-9]{1,3})|asse|aue|brs|euw|use)$/;
const legacyPattern = /^magpie-wan-[a-f0-9]{32}$/;
const generationPattern = /^[a-f0-9]{32}$/;
const aborter = new AbortController();
const cancellation = new CancellationTokenSource();

type Code = 'auth' | 'forbidden' | 'not_found' | 'ports' | 'proxy' | 'disconnected' | 'reconnect_timeout' | 'host_conflict' | 'prepare' | 'host' | 'protocol' | 'dispose' | 'bad_request' | 'conflict' | 'rate_limit' | 'service' | 'network' | 'tls' | 'timeout' | 'runtime' | 'create' | 'read' | 'update' | 'identity' | 'port_options' | 'host_token' | 'redirect';
type Config = { version: 2; id: string; clusterId: string; port: number };
type Phase = 'new' | 'preparing' | 'prepared' | 'hosting' | 'running' | 'closing';
class Failure extends Error {
    constructor(readonly code: Code) { super(code); }
}
let phase: Phase = 'new';
let generation = '';
let token = '';
let authProvider = 'github';
let credentialRevision = 0;
let credentialUpdating = false;
let ownedPort = 0;
let preparedEndpoint = '';
let config: Config | undefined;
let tunnel: Tunnel | undefined;
let client: any;
let host: any;
let agent: any;
let currentOperation: Promise<void> = Promise.resolve();
let shutdownTask: Promise<void> | undefined;
let errorSent = false;
let preparingOperation: Code = 'prepare';
let disposingHost = false;
let relayRecovery: RelayRecovery | undefined;
const bridges = new Set<Duplex>();

function object(value: unknown): value is Record<string, any> {
    return value !== null && typeof value === 'object' && !Array.isArray(value);
}
function keys(value: Record<string, any>, allowed: string[], required = allowed): void {
    if (Object.keys(value).some(key => !allowed.includes(key)) || required.some(key => !Object.prototype.hasOwnProperty.call(value, key))) {
        throw new Failure('protocol');
    }
}
function validPort(port: unknown): port is number {
    return Number.isInteger(port) && Number(port) > 0 && Number(port) <= 65535;
}
function live(): void {
    if (phase === 'closing' || aborter.signal.aborted) throw new Failure('disconnected');
}
function emit(event: string, fields: Record<string, unknown> = {}): void {
    if (!generation) return;
    const data = JSON.stringify({ version: wireVersion, generation, event, ...fields }) + '\n';
    if (Buffer.byteLength(data) > lineLimit) throw new Failure('protocol');
    process.stdout.write(data);
}
function codeFor(error: unknown, fallback: Code): Code {
    if (error instanceof Failure) return error.code;
    const status = (error as any)?.response?.status ?? (error as any)?.errorContext?.statusCode;
    if (status === 401) return 'auth';
    if (status === 403) return 'forbidden';
    if (status === 404) return 'not_found';
    if (status === 407) return 'proxy';
    if (status === 400) return 'bad_request';
    if (status === 409) return 'conflict';
    if (status === 429) return 'rate_limit';
    if (Number.isInteger(status) && status >= 500 && status <= 599) return 'service';
    // Only fixed categories cross IPC. Never expose Error.message, URLs,
    // request/response bodies, headers, or SDK error objects.
    const code = (error as any)?.code;
    if (['ETIMEDOUT', 'ECONNABORTED', 'ERR_CANCELED'].includes(code)) return 'timeout';
    if (['ENOTFOUND', 'EAI_AGAIN', 'ECONNREFUSED', 'ECONNRESET', 'ENETUNREACH', 'EHOSTUNREACH'].includes(code)) return 'network';
    if (['CERT_HAS_EXPIRED', 'DEPTH_ZERO_SELF_SIGNED_CERT', 'SELF_SIGNED_CERT_IN_CHAIN', 'UNABLE_TO_VERIFY_LEAF_SIGNATURE', 'UNABLE_TO_GET_ISSUER_CERT_LOCALLY', 'ERR_TLS_CERT_ALTNAME_INVALID'].includes(code)) return 'tls';
    if (error instanceof TypeError) return 'runtime';
    return fallback;
}
function fail(code: Code): void {
    if (phase === 'closing') return;
    if (!errorSent) {
        errorSent = true;
        emit('error', { code });
    }
    void shutdown(1);
}

// JSON.parse alone silently takes the last duplicate key. Scan every object
// first, with bounded nesting, before accepting a credential-bearing message.
function parseLine(data: Buffer): Record<string, any> {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(data);
    let at = 0;
    function ws(): void { while (at < text.length && /[\x20\t\r\n]/.test(text[at])) at++; }
    function string(): string {
        const begin = at++;
        while (at < text.length) {
            const c = text[at++];
            if (c === '\\') { at++; continue; }
            if (c === '"') return JSON.parse(text.slice(begin, at));
        }
        throw new Failure('protocol');
    }
    function value(depth: number): void {
        if (depth > 12) throw new Failure('protocol');
        ws();
        const c = text[at];
        if (c === '{') {
            at++; ws();
            const seen = new Set<string>();
            if (text[at] === '}') { at++; return; }
            for (;;) {
                ws();
                if (text[at] !== '"') throw new Failure('protocol');
                const key = string();
                if (seen.has(key)) throw new Failure('protocol');
                seen.add(key); ws();
                if (text[at++] !== ':') throw new Failure('protocol');
                value(depth + 1); ws();
                const end = text[at++];
                if (end === '}') return;
                if (end !== ',') throw new Failure('protocol');
            }
        }
        if (c === '[') {
            at++; ws();
            if (text[at] === ']') { at++; return; }
            for (;;) {
                value(depth + 1); ws();
                const end = text[at++];
                if (end === ']') return;
                if (end !== ',') throw new Failure('protocol');
            }
        }
        if (c === '"') { string(); return; }
        const match = /^(?:true|false|null|-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)/.exec(text.slice(at));
        if (!match) throw new Failure('protocol');
        at += match[0].length;
    }
    value(0); ws();
    if (at !== text.length) throw new Failure('protocol');
    const result = JSON.parse(text);
    if (!object(result)) throw new Failure('protocol');
    return result;
}

function identity(value: Tunnel, portNumber = ownedPort): Config {
    if (!object(value) || typeof value.tunnelId !== 'string' || !idPattern.test(value.tunnelId) ||
        typeof value.clusterId !== 'string' || !clusterPattern.test(value.clusterId)) throw new Failure('identity');
    return { version: 2, id: value.tunnelId, clusterId: value.clusterId, port: portNumber };
}
function sameIdentity(value: Tunnel): void {
    const found = identity(value);
    if (!config || found.id !== config.id || found.clusterId !== config.clusterId) throw new Failure('identity');
}
function onePort(value: Tunnel, portNumber = ownedPort): TunnelPort {
    if (!Array.isArray(value.ports) || value.ports.length !== 1 || !object(value.ports[0]) || value.ports[0].portNumber !== portNumber) {
        throw new Failure('ports');
    }
    return value.ports[0];
}
function safeAnonymousACL(value: any): void {
    if (value === undefined) return;
    if (!object(value) || !Array.isArray(value.entries)) throw new Failure('ports');
    for (const entry of value.entries) {
        if (!object(entry) || !Array.isArray(entry.scopes)) throw new Failure('ports');
        if (entry.type === TunnelAccessControlEntryType.Anonymous && !entry.isDeny &&
            entry.scopes.some((scope: unknown) => scope !== TunnelAccessScopes.Connect)) throw new Failure('ports');
    }
}
function endpoint(value: Tunnel, required: boolean, portNumber = ownedPort): string {
    const port = onePort(value, portNumber);
    const forwarding = port.portForwardingUris;
    if (forwarding !== undefined && (!Array.isArray(forwarding) || forwarding.length > 16)) throw new Failure('host');
    const uris: unknown[] = [...(forwarding || [])];
    if (Array.isArray(value.endpoints)) {
        if (value.endpoints.length > 16) throw new Failure('host');
        for (const entry of value.endpoints) {
            if (object(entry) && typeof entry.portUriFormat === 'string') uris.push(entry.portUriFormat.replace('{port}', String(portNumber)));
        }
    }
    if (uris.length === 0) {
        if (required) throw new Failure('host');
        return '';
    }
    for (const raw of uris) {
        if (typeof raw !== 'string' || raw.length > 1024) throw new Failure('host');
        let url: URL;
        try { url = new URL(raw); } catch { throw new Failure('host'); }
        if (url.protocol !== 'https:') continue;
        if (url.username || url.password || url.search || url.hash || (url.pathname !== '/' && url.pathname !== '')) continue;
        const labels = url.hostname.split('.');
        if (labels.length !== 4 || labels[2] !== 'devtunnels' || labels[3] !== 'ms' || !/^[a-z0-9]{1,30}$/.test(labels[1])) continue;
        let name = labels[0];
        if (url.port) {
            if (url.port !== String(portNumber)) continue;
        } else {
            if (!name.endsWith('-' + portNumber)) continue;
            name = name.slice(0, -(String(portNumber).length + 1));
        }
        if (!/^[a-z0-9](?:[a-z0-9-]{0,58}[a-z0-9])?$/.test(name)) continue;
        return url.origin;
    }
    throw new Failure('host');
}
// TunnelOptions.IsInspectionEnabled is a non-nullable C# bool serialized with
// WhenWritingDefault: a false value is omitted from real management responses.
// Missing is false; null, strings, true, and a tunnel-level enabled flag are not.
export function validatePortOptions(port: TunnelPort, tunnelOptions: unknown): void {
    const opts = port.options;
    const disabled = (value: unknown) => value === undefined || value === false;
    if (port.protocol !== TunnelProtocol.Http || !object(opts) || opts.isHostHeaderUnchanged !== true ||
        opts.isOriginHeaderUnchanged !== true || opts.requestTimeoutSeconds !== 0 || !disabled(opts.isInspectionEnabled) ||
        (tunnelOptions !== undefined && (!object(tunnelOptions) || !disabled(tunnelOptions.isInspectionEnabled)))) {
        throw new Failure('port_options');
    }
}

function desiredPort(): TunnelPort {
    return {
        portNumber: ownedPort,
        protocol: TunnelProtocol.Http,
        options: { isHostHeaderUnchanged: true, isOriginHeaderUnchanged: true, requestTimeoutSeconds: 0, isInspectionEnabled: false },
        accessControl: { entries: [{ type: TunnelAccessControlEntryType.Anonymous, subjects: [], scopes: [TunnelAccessScopes.Connect] }] },
    };
}

// Explicit agents handle both management HTTPS and the SDK's websocket HTTPS
// request. Axios must not independently consult proxy environment variables.
class ManagementClient extends TunnelManagementHttpClient {
    async axiosRequest(request: any): Promise<any> {
        const cleanup = disposingHost && request.method === 'DELETE' && typeof request.url === 'string' &&
            /\/endpoints\/[a-f0-9-]+-relay\?/.test(request.url);
        if (!cleanup) live();
        const lifetime = cleanup ? AbortSignal.timeout(750) : aborter.signal;
        const signal = request.signal ? AbortSignal.any([request.signal, lifetime]) : lifetime;
        return super.axiosRequest({
            ...request, signal, proxy: false, timeout: cleanup ? 750 : request.timeout,
            maxContentLength: 1024 * 1024, maxBodyLength: 1024 * 1024, maxRedirects: 3,
            beforeRedirect: (next: any) => {
                if (next.protocol !== 'https:' || !/^(?:global|[a-z0-9]+)\.rel\.tunnels\.api\.visualstudio\.com$/.test(next.hostname || '') ||
                    next.auth || (next.port && String(next.port) !== '443')) throw new Failure('redirect');
            },
        });
    }
}

function managementOptions() {
    // Preparation verifies nested port ACLs as well as the tunnel's ACL. Ask
    // explicitly for both instead of relying on single-tunnel response defaults.
    return { includePorts: true, includeAccessControl: true, tokenScopes: [TunnelAccessScopes.Host] };
}

async function prepare(message: Record<string, any>): Promise<void> {
    keys(message, ['version', 'generation', 'op', 'port', 'fresh', 'config', 'token', 'clientID', 'proxy', 'authProvider'], ['version', 'generation', 'op', 'port', 'fresh', 'config', 'token', 'clientID', 'proxy']);
    authProvider = message.authProvider || 'github';
    if (!['github', 'microsoft'].includes(authProvider)) throw new Failure('protocol');
    if (!validPort(message.port) || typeof message.fresh !== 'boolean' || typeof message.token !== 'string' ||
        !/^[\x21-\x7e]{1,8192}$/.test(message.token) || message.clientID !== (authProvider === 'microsoft' ? microsoftClientID : serviceClientID) || typeof message.proxy !== 'string') {
        throw new Failure('protocol');
    }
    ownedPort = message.port;
    token = message.token;
    message.token = '';
    if (message.proxy) {
        let proxy: URL;
        try { proxy = new URL(message.proxy); } catch { throw new Failure('proxy'); }
        if (!['http:', 'https:'].includes(proxy.protocol) || !proxy.hostname || proxy.search || proxy.hash ||
            (proxy.pathname !== '/' && proxy.pathname !== '') || message.proxy.length > 8192) throw new Failure('proxy');
        agent = new HttpsProxyAgent(proxy);
    }
    message.proxy = '';
    client = new ManagementClient({ name: 'magpie-wan', version: '1' }, ManagementApiVersions.Version20230927preview,
        async () => { if (!disposingHost) live(); return serviceAuthorization(authProvider, token); }, undefined, agent);
    client.enableEventsReporting = false;
    client.trace = () => {};
    const options = managementOptions();
    if (message.fresh) {
        if (message.config !== null) throw new Failure('protocol');
        // Supplying our own fixed ID avoids createTunnel's generated-ID retries:
        // an ambiguous network result must never silently create a replacement.
        const id = 'magpie-wan-' + randomBytes(16).toString('hex');
        preparingOperation = 'create';
        tunnel = await client.createTunnel({ tunnelId: id, ports: [desiredPort()] }, options, cancellation.token);
        live();
        config = identity(tunnel!);
        if (config.id !== id) throw new Failure('identity');
    } else {
        const saved = message.config;
        if (!object(saved) || saved.port !== ownedPort) throw new Failure('protocol');
        if (saved.version === 2) {
            keys(saved, ['version', 'id', 'clusterId', 'port']);
            if (typeof saved.id !== 'string' || !idPattern.test(saved.id) || typeof saved.clusterId !== 'string' || !clusterPattern.test(saved.clusterId)) {
                throw new Failure('protocol');
            }
            config = { version: 2, id: saved.id, clusterId: saved.clusterId, port: ownedPort };
        } else if (saved.version === 1) {
            keys(saved, ['version', 'id', 'port']);
            if (typeof saved.id !== 'string' || !legacyPattern.test(saved.id)) throw new Failure('protocol');
            const candidates: Tunnel[] = await client.listTunnels(undefined, undefined, options, cancellation.token);
            live();
            if (!Array.isArray(candidates) || candidates.length > 10000) throw new Failure('prepare');
            const matching = candidates.filter(value => object(value) && (value.tunnelId === saved.id || value.name === saved.id));
            if (matching.length !== 1) throw new Failure('not_found');
            config = identity(matching[0]);
        } else {
            throw new Failure('protocol');
        }
    }
    live();
    const target = { tunnelId: config!.id, clusterId: config!.clusterId };
    preparingOperation = 'read';
    tunnel = await client.getTunnel(target, options, cancellation.token);
    live();
    if (!tunnel) throw new Failure('not_found');
    sameIdentity(tunnel);
    onePort(tunnel);
    safeAnonymousACL(tunnel.accessControl);
    safeAnonymousACL(tunnel.ports![0].accessControl);
    preparingOperation = 'update';
    await client.updateTunnelPort(target, desiredPort(), options, cancellation.token);
    live();
    preparingOperation = 'read';
    tunnel = await client.getTunnel(target, options, cancellation.token);
    live();
    if (!tunnel) throw new Failure('not_found');
    sameIdentity(tunnel);
    const port = onePort(tunnel);
    validatePortOptions(port, tunnel.options);
    if (!object(tunnel.accessTokens) || typeof tunnel.accessTokens[TunnelAccessScopes.Host] !== 'string' ||
        !tunnel.accessTokens[TunnelAccessScopes.Host]) throw new Failure('host_token');
    safeAnonymousACL(tunnel.accessControl);
    safeAnonymousACL(port.accessControl);
    const publicConnect = port.accessControl?.entries.some(entry => entry.type === TunnelAccessControlEntryType.Anonymous &&
        !entry.isDeny && !entry.isInverse && entry.scopes.length === 1 && entry.scopes[0] === TunnelAccessScopes.Connect);
    if (!publicConnect) throw new Failure('ports');
    preparedEndpoint = endpoint(tunnel, false);
    phase = 'prepared';
    emit('prepared', { config, endpoint: preparedEndpoint });
}

export function createPortBridge(port: number, active: () => boolean, signal: AbortSignal, streams = new Set<Duplex>()) {
    if (!validPort(port)) throw new Failure('ports');
    const destroyAll = () => { for (const stream of streams) stream.destroy(); streams.clear(); };
    signal.addEventListener('abort', destroyAll, { once: true });
    return {
        close: destroyAll,
        handle(event: ForwardedPortConnectingEventArgs): void {
            // V1 emits isIncoming=false and no cancellation even for inbound streams.
            // Neither field is an authority. The fixed port and this lifetime are.
            const remote = event.stream;
            if (!remote || typeof remote.destroy !== 'function') throw new Failure('host');
            if (!active() || event.port !== port || signal.aborted || event.cancellation?.isCancellationRequested) {
                remote.on('error', () => {});
                remote.destroy();
                return;
            }
            streams.add(remote);
            let socket: Socket;
            let registration: { dispose(): void } | undefined;
            let destroyed = false;
            const close = () => {
                if (destroyed) return;
                destroyed = true;
                registration?.dispose();
                remote.destroy();
                socket?.destroy();
                streams.delete(remote);
                if (socket) streams.delete(socket);
            };
            remote.on('error', close);
            remote.once('close', close);
            remote.pause();
            // No remote address, SSH host field, metadata port or alias reaches dial.
            socket = createConnection({ host: '127.0.0.1', family: 4, port });
            streams.add(socket);
            socket.on('error', close);
            socket.once('close', close);
            registration = event.cancellation?.onCancellationRequested(close);
            socket.once('connect', () => {
                if (destroyed || signal.aborted || !active() || event.cancellation?.isCancellationRequested) {
                    close(); return;
                }
                // pipe supplies backpressure without a whole-request timeout.
                // Close/error tears down both; TCP half-close is SDK-defined.
                remote.pipe(socket);
                socket.pipe(remote);
            });
        },
    };
}

export class PortHost extends TunnelRelayTunnelHost {
    constructor(management: any, private readonly fixedPort: number, private readonly active: () => boolean) {
        super(management, () => {});
    }
    protected tunnelChanged(): void {
        super.tunnelChanged();
        // The V1 host authorizes direct-tcpip requests against this local list.
        // Keep refreshed metadata pinned too, not only port advertisement.
        const current = this.tunnel;
        if (current && Array.isArray(current.ports)) current.ports = current.ports.filter((port: TunnelPort) => port.portNumber === this.fixedPort);
    }
    protected async forwardPort(service: PortForwardingService, port: TunnelPort): Promise<void> {
        // refreshPorts may see externally added ports after the initial check.
        // They are never advertised by this host, even before a stream arrives.
        if (port.portNumber !== this.fixedPort || !this.active()) return;
        await super.forwardPort(service, port);
    }
}
// Tests use the exact bundled SSH types, not a second package instance that
// would fail the SDK's instanceof checks. Requiring this module starts no work.
export const sdkFixture = { ...require('@microsoft/dev-tunnels-ssh'), ...require('@microsoft/dev-tunnels-ssh-tcp'),
    TunnelRelayTunnelHost, TunnelProtocol, TunnelAccessScopes, ConnectionStatus, ManagementClient, ManagementApiVersions, HttpsProxyAgent, managementOptions };
export function serviceAuthorization(provider: string, credential: string): string {
    if (!['github', 'microsoft'].includes(provider) || !/^[\x21-\x7e]{1,8192}$/.test(credential)) throw new Failure('protocol');
    return (provider === 'microsoft' ? 'Bearer ' : 'github ') + credential;
}
export const protocolFixture = { parseLine, endpoint, codeFor, serviceAuthorization };

// Only the SDK's existing connection may recover. This never starts a helper,
// creates a tunnel, changes identity, or survives parent cancellation. A brief
// transport outage does not revoke the parent's still-bounded credential lease.
export class RelayRecovery {
    private timer: ReturnType<typeof setTimeout> | undefined;
    private retryTimer: ReturnType<typeof setTimeout> | undefined;
    private stopped = false;
    private inFlight = false;
    private until = 0;
    private lastAttempt = -Infinity;
    private attempts = 0;
    private pending: Promise<void> = Promise.resolve();
    private readonly subscriptions: any[] = [];
    constructor(private readonly relay: any, private readonly running: () => boolean,
        private readonly signal: AbortSignal, private readonly failure: (code: Code) => void,
        private readonly reconnect: () => Promise<void>, private readonly schedule = setTimeout,
        private readonly cancelTimer = clearTimeout, private readonly now = () => performance.now(),
        private readonly progress: (value: { attempts: number; retryInMs: number; retrying: boolean } | null) => void = () => {}) {
        this.subscriptions.push(relay.connectionStatusChanged((event: any) => {
            if (this.stopped || signal.aborted) return;
            if (event.status === ConnectionStatus.Connected) {
                const recovering = this.timer !== undefined;
                this.reset();
                if (recovering) this.progress(null);
                return;
            }
            if (event.status !== ConnectionStatus.Disconnected) return;
            const code = this.category(relay.disconnectError);
            if (!running() || !this.recoverable(code)) { this.terminate(code); return; }
            this.begin(); this.queue();
        }));
        // One SDK connect call is one attempt. The owner controls both cadence
        // and deadline, including SDK errors whose own retry loop ends early.
        this.subscriptions.push(relay.retryingTunnelConnection((event: any) => { event.retry = false; }));
        signal.addEventListener('abort', this.onAbort, { once: true });
        if (signal.aborted) this.dispose();
    }
    private category(error: any): Code {
        const reasons = require('@microsoft/dev-tunnels-ssh').SshDisconnectReason;
        if (this.relay.disconnectReason === reasons.tooManyConnections) return 'host_conflict';
        return codeFor(error, error?.reason === reasons.connectionLost || this.relay.disconnectReason === reasons.connectionLost ? 'network' : 'disconnected');
    }
    private recoverable(code: Code): boolean { return ['network', 'timeout', 'service'].includes(code); }
    private begin(): void {
        if (this.timer !== undefined) return;
        this.until = this.now() + 300000;
        this.timer = this.schedule(() => {
            if (!this.stopped && !this.signal.aborted) this.terminate('reconnect_timeout');
        }, 300000);
    }
    private queue(): void {
        if (this.stopped || this.signal.aborted || this.inFlight || this.retryTimer !== undefined) return;
        // performance.now() has sub-millisecond precision; the Go wire schema
        // requires integer milliseconds. Round up to preserve the minimum gap.
        const delay = Math.ceil(Math.max(0, 20000 - (this.now() - this.lastAttempt)));
        this.progress({ attempts: this.attempts, retryInMs: delay, retrying: false });
        this.retryTimer = this.schedule(() => {
            this.retryTimer = undefined;
            if (this.stopped || this.signal.aborted || !this.running()) return;
            if (this.now() >= this.until) { this.terminate('reconnect_timeout'); return; }
            this.lastAttempt = this.now(); this.inFlight = true;
            this.attempts++;
            this.progress({ attempts: this.attempts, retryInMs: 0, retrying: true });
            this.pending = Promise.resolve().then(() => {
                if (!this.stopped && !this.signal.aborted && this.running()) return this.reconnect();
            }).catch(error => {
                if (this.stopped || this.signal.aborted) return;
                const code = this.category(error);
                if (!this.recoverable(code)) this.terminate(code);
            }).finally(() => {
                this.inFlight = false;
                if (!this.stopped && !this.signal.aborted && this.timer !== undefined) this.queue();
            });
        }, delay);
    }
    private readonly onAbort = () => this.dispose();
    private reset(): void {
        if (this.timer !== undefined) this.cancelTimer(this.timer);
        if (this.retryTimer !== undefined) this.cancelTimer(this.retryTimer);
        this.timer = undefined; this.retryTimer = undefined; this.until = 0; this.lastAttempt = -Infinity; this.attempts = 0;
    }
    private terminate(code: Code): void { this.dispose(); this.failure(code); }
    wait(): Promise<void> { return this.pending; }
    dispose(): void {
        if (this.stopped) return;
        this.stopped = true; this.reset();
        this.signal.removeEventListener('abort', this.onAbort);
        for (const subscription of this.subscriptions) subscription?.dispose();
    }
}
// Replacing metadata updates the SDK's host access token without reconnecting
// existing streams. Revalidate the same boundary as initial preparation.
export async function renewHostCredentials(management: any, activeHost: any, target: Config, check: () => void, expectedEndpoint: string): Promise<Tunnel> {
    const current: Tunnel | null = await management.getTunnel({ tunnelId: target.id, clusterId: target.clusterId }, managementOptions(), cancellation.token);
    check();
    if (!current) throw new Failure('not_found');
    const actual = identity(current, target.port);
    if (actual.id !== target.id || actual.clusterId !== target.clusterId || actual.port !== target.port) throw new Failure('identity');
    const port = onePort(current, target.port);
    validatePortOptions(port, current.options);
    safeAnonymousACL(current.accessControl);
    safeAnonymousACL(port.accessControl);
    if (!port.accessControl?.entries.some(entry => entry.type === TunnelAccessControlEntryType.Anonymous && !entry.isDeny && !entry.isInverse && entry.scopes.length === 1 && entry.scopes[0] === TunnelAccessScopes.Connect)) throw new Failure('ports');
    if (!object(current.accessTokens) || typeof current.accessTokens[TunnelAccessScopes.Host] !== 'string' || !current.accessTokens[TunnelAccessScopes.Host]) throw new Failure('host_token');
    const url = endpoint(current, true, target.port);
    if (url !== expectedEndpoint) throw new Failure('host');
    activeHost.tunnel = current;
    return current;
}

async function updateCredentials(message: Record<string, any>): Promise<void> {
    keys(message, ['version', 'generation', 'op', 'token', 'revision', 'authProvider'], ['version', 'generation', 'op', 'token', 'revision']);
    if ((message.authProvider || 'github') !== authProvider) throw new Failure('protocol');
    if (typeof message.token !== 'string' || !/^[\x21-\x7e]{1,8192}$/.test(message.token) || !Number.isSafeInteger(message.revision) || message.revision !== credentialRevision + 1) throw new Failure('protocol');
    token = message.token;
    message.token = '';
    tunnel = await renewHostCredentials(client, host, config!, live, preparedEndpoint);
    live();
    credentialRevision = message.revision;
    credentialUpdating = false;
    emit('credentials', { revision: credentialRevision });
}

async function startHost(): Promise<void> {
    live();
    if (!client || !tunnel || !config) throw new Failure('protocol');
    const active = () => !aborter.signal.aborted && (phase === 'hosting' || phase === 'running');
    host = new PortHost(client, ownedPort, active);
    host.forwardConnectionsToLocalPorts = false;
    host.httpAgent = agent;
    const bridge = createPortBridge(ownedPort, active, aborter.signal, bridges);
    host.forwardedPortConnecting(bridge.handle);
    relayRecovery = new RelayRecovery(host, () => phase === 'running', aborter.signal, fail,
        () => host.connect(host.tunnel, { enableRetry: false, enableReconnect: false, httpAgent: agent }, cancellation.token),
        setTimeout, clearTimeout, () => performance.now(),
        progress => progress ? emit('recovery', progress) : emit('recovered'));
    await host.connect(tunnel, { enableRetry: false, enableReconnect: false, httpAgent: agent }, cancellation.token);
    live();
    if (host.connectionProtocol !== TunnelRelayTunnelHost.webSocketSubProtocol || host.connectionStatus !== ConnectionStatus.Connected) throw new Failure('host');
    const current: Tunnel | null = await client.getTunnel({ tunnelId: config.id, clusterId: config.clusterId }, { includePorts: true }, cancellation.token);
    live();
    if (!current) throw new Failure('not_found');
    sameIdentity(current);
    const url = endpoint(current, true);
    if (preparedEndpoint && preparedEndpoint !== url) throw new Failure('host');
    preparedEndpoint = url;
    phase = 'running';
    emit('ready', { endpoint: url });
}

function shutdown(exitCode: number): Promise<void> {
    if (shutdownTask) return shutdownTask;
    phase = 'closing';
    relayRecovery?.dispose();
    aborter.abort();
    cancellation.cancel();
    process.stdin.pause();
    for (const stream of bridges) stream.destroy();
    bridges.clear();
    shutdownTask = (async () => {
        // Join preparation/connection before disposing, so a late operation
        // cannot create an endpoint after dispose. The parent bounds this join
        // with proc.Tree.Stop and retains the loopback socket until Wait returns.
        await currentOperation.catch(() => {});
        await relayRecovery?.wait();
        disposingHost = true;
        try { if (host) await host.dispose(); } catch { exitCode = 1; }
        disposingHost = false;
        try { if (client) await client.dispose(); } catch { exitCode = 1; }
        try { agent?.destroy(); } catch { exitCode = 1; }
        token = '';
        tunnel = undefined;
        cancellation.dispose();
        process.exit(exitCode);
    })();
    return shutdownTask;
}
function receive(message: Record<string, any>): void {
    if (message.version !== wireVersion || typeof message.generation !== 'string' || !generationPattern.test(message.generation)) throw new Failure('protocol');
    if (!generation && phase === 'new') generation = message.generation;
    if (message.generation !== generation) throw new Failure('protocol');
    if (message.op === 'prepare' && phase === 'new') {
        phase = 'preparing';
        currentOperation = prepare(message);
        void currentOperation.catch(error => fail(codeFor(error, preparingOperation)));
    } else if (message.op === 'host' && phase === 'prepared') {
        keys(message, ['version', 'generation', 'op']);
        phase = 'hosting';
        currentOperation = startHost();
        void currentOperation.catch(error => fail(codeFor(error, 'host')));
    } else if (message.op === 'credentials' && phase === 'running' && !credentialUpdating) {
        credentialUpdating = true;
        currentOperation = updateCredentials(message);
        void currentOperation.catch(error => fail(codeFor(error, 'host')));
    } else {
        throw new Failure('protocol');
    }
}

if (require.main === module) {
let pending = Buffer.alloc(0);
process.stdin.on('data', (chunk: Buffer) => {
    if (phase === 'closing') return;
    try {
        let start = 0;
        for (let at = 0; at < chunk.length; at++) {
            if (chunk[at] !== 10) continue;
            const length = pending.length + at - start;
            if (length + 1 > lineLimit || length === 0) throw new Failure('protocol');
            const line = Buffer.concat([pending, chunk.subarray(start, at)], length);
            pending = Buffer.alloc(0);
            start = at + 1;
            receive(parseLine(line));
            line.fill(0);
            if (phase === 'closing') return;
        }
        if (start < chunk.length) {
            if (pending.length + chunk.length - start + 1 > lineLimit) throw new Failure('protocol');
            pending = Buffer.concat([pending, chunk.subarray(start)]);
        }
    } catch {
        fail('protocol');
    }
});
process.stdin.on('end', () => { pending.fill(0); void shutdown(0); });
process.stdin.on('error', () => fail('protocol'));
process.stdout.on('error', () => { void shutdown(1); });
process.on('SIGINT', () => { void shutdown(0); });
process.on('SIGTERM', () => { void shutdown(0); });
process.on('uncaughtException', () => fail('host'));
process.on('unhandledRejection', () => fail('host'));
}
