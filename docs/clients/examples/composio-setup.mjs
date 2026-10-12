// Manual owner-run example. Running it registers/connects/syncs in your project.
// Source-checked with @composio/core 0.22.0; no account execution/model turn.
import { Composio } from '@composio/core';

let stage = 'input validation';
try {
  const required = (name) => {
    const value = process.env[name];
    if (!value) throw new Error('Required environment value is missing');
    return value;
  };
  const publicURL = required('WA_INDEXER_PUBLIC_URL');
  const userID = required('WA_INDEXER_COMPOSIO_USER_ID');
  const rawToken = required('WA_INDEXER_CLIENT_TOKEN');
  const url = new URL(publicURL);
  if (url.protocol !== 'https:' || url.username || url.password ||
      url.pathname !== '/mcp' || url.search || url.hash ||
      url.href !== publicURL || /^Bearer\s/i.test(rawToken)) {
    throw new Error('Use the app-configured URL and raw credential');
  }
  const composio = new Composio({
    apiKey: required('COMPOSIO_API_KEY'),
    userApiKey: null,
    orgApiKey: null,
    baseURL: 'https://backend.composio.dev',
    logLevel: 'silent',
    allowTracking: false,
    disableVersionCheck: true,
  });
  const requestOptions = () => ({ signal: AbortSignal.timeout(30000) });

  stage = 'toolkit registration';
  const toolkit = await composio.experimental.customToolkits.upsert({
    slug: 'WA_INDEXER',
    toolkitConfig: {
      name: 'WA Indexer',
      appUrl: publicURL,
      authSchemes: [{
        mode: 'API_KEY',
        headers: { Authorization: 'Bearer {{generic_api_key}}' },
        apiKeyField: { displayName: 'WA Indexer token',
          description: 'Paste the raw per-client token; omit the Bearer prefix.' },
      }],
    },
  }, requestOptions());
  console.log(JSON.stringify({ toolkitSlug: toolkit.slug }));

  stage = 'auth-config creation';
  const auth = await composio.authConfigs.create(toolkit.slug, {
    type: 'use_custom_auth',
    authScheme: 'API_KEY',
    credentials: {},
    isEnabledForToolRouter: true,
    name: 'WA Indexer owner API key',
  }, requestOptions());
  console.log(JSON.stringify({ authConfigID: auth.id }));

  stage = 'credential connection';
  const connection = await composio.connectedAccounts.initiate(userID, auth.id, {
    config: { authScheme: 'API_KEY',
      val: { status: 'ACTIVE', generic_api_key: rawToken } },
  }, requestOptions());
  console.log(JSON.stringify({ connectedAccountID: connection.id }));
  const active = await composio.connectedAccounts.get(connection.id, requestOptions());
  if (active.status !== 'ACTIVE') throw new Error('Connection is not active');

  stage = 'authenticated tool sync';
  const synced = await composio.experimental.customToolkits.sync(toolkit.slug,
    { connectedAccountId: connection.id }, requestOptions());
  console.log(JSON.stringify({ syncedCount: synced.syncedCount,
    toolkitVersion: synced.version }));

  stage = 'session configuration';
  const session = await composio.sessions.create(userID, {
    toolkits: [toolkit.slug],
    connectedAccounts: { [toolkit.slug]: [connection.id] },
    manageConnections: false,
    sandbox: { enable: false },
  }, requestOptions());
  console.log(JSON.stringify({ sessionID: session.sessionId }));
} catch {
  // SDK errors/account responses can contain credentials: never print them.
  console.error(`Setup stopped during ${stage}. Keep printed IDs for recovery.`);
  process.exitCode = 1;
} finally {
  delete process.env.COMPOSIO_API_KEY;
  delete process.env.WA_INDEXER_CLIENT_TOKEN;
}
