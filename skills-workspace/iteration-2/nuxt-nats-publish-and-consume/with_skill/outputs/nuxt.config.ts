export default defineNuxtConfig({
  modules: ['nuxt-nats'],
  nats: {
    // Prod: override via NUXT_NATS_SERVERS (comma-separated, ALL cluster nodes).
    servers: ['nats://localhost:4222'],
    // Auth is injected via env in prod: NUXT_NATS_USER_JWT + NUXT_NATS_NKEY_SEED.
    streams: [
      // DESCRIBES the stream; IaC owns it. provision:'never' => the app never
      // creates/edits it (no racing replicas on rolling deploys).
      { name: 'ORDERS', subjects: ['orders.>'], provision: 'never' },
    ],
  },
})
