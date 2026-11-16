// The leading `import type` line is required, or the augmentation silently
// does not apply. Types every jsPublish subject + payload.
import type {} from 'nuxt-nats'

declare module 'nuxt-nats' {
  interface NatsEvents {
    'orders.created': { id: string; total: number }
  }
}
