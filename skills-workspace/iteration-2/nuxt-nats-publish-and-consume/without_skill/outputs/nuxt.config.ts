export default defineNuxtConfig({
  modules: ['nuxt-nats'],

  // nuxt-nats options -> used to open the connection on the Nitro server.
  nats: {
    servers: process.env.NATS_URL || 'nats://localhost:4222',
    // Enable the connection on the server (Nitro) side; we don't need NATS in the browser here.
    client: false,
  },

  runtimeConfig: {
    nats: {
      stream: 'ORDERS',
      durable: 'order-processor',
    },
  },
})
