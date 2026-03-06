# Python Examples

Complete JetStream examples using `nats-py` (pip package `nats-py`).

## Connection and JetStream Client

```python
import asyncio
import json
import signal
from nats.aio.client import Client as NATS
from nats.js.api import (
    StreamConfig, ConsumerConfig, RetentionPolicy, StorageType,
    DiscardPolicy, AckPolicy, DeliverPolicy, ReplayPolicy
)


async def main():
    nc = NATS()

    async def error_cb(e):
        print(f"nats error: {e}")

    async def disconnected_cb():
        print("disconnected from NATS")

    async def reconnected_cb():
        print(f"reconnected to {nc.connected_url.netloc}")

    await nc.connect(
        servers=["nats://localhost:4222"],
        error_cb=error_cb,
        disconnected_cb=disconnected_cb,
        reconnected_cb=reconnected_cb,
        max_reconnect_attempts=-1,
        reconnect_time_wait=2,
    )

    js = nc.jetstream()
    await nc.drain()


asyncio.run(main())
```

## Stream Creation

```python
async def create_stream(js):
    await js.add_stream(StreamConfig(
        name="ORDERS",
        subjects=["orders.>"],
        retention=RetentionPolicy.LIMITS,
        max_age=30 * 24 * 60 * 60,  # 30 days in seconds
        max_bytes=5 * 1024 * 1024 * 1024,
        max_msgs=-1,
        storage=StorageType.FILE,
        num_replicas=3,
        discard=DiscardPolicy.OLD,
        duplicate_window=120,
        max_msgs_per_subject=1000,
    ))
```

## Publishing

```python
# Simple publish
async def publish_order(js, order: dict):
    ack = await js.publish(
        "orders.created",
        json.dumps(order).encode(),
    )
    print(f"published seq={ack.seq} stream={ack.stream}")


# Publish with deduplication
async def publish_idempotent(js, order_id: str, order: dict):
    ack = await js.publish(
        "orders.created",
        json.dumps(order).encode(),
        headers={"Nats-Msg-Id": order_id},
    )
    return ack


# Publish with retry
async def publish_with_retry(js, subject: str, data: dict, max_retries: int = 3):
    for attempt in range(max_retries):
        try:
            return await js.publish(subject, json.dumps(data).encode())
        except Exception as e:
            print(f"publish attempt {attempt + 1} failed: {e}")
            if attempt == max_retries - 1:
                raise
            await asyncio.sleep(1 * (attempt + 1))
```

## Pull Consumer

```python
async def pull_consumer(js):
    await js.add_consumer("ORDERS", ConsumerConfig(
        durable_name="order-processor",
        ack_policy=AckPolicy.EXPLICIT,
        ack_wait=30,
        max_deliver=5,
        max_ack_pending=1000,
        filter_subject="orders.>",
        deliver_policy=DeliverPolicy.ALL,
    ))

    psub = await js.pull_subscribe("orders.>", durable="order-processor")

    while True:
        try:
            msgs = await psub.fetch(batch=100, timeout=5)
        except TimeoutError:
            continue
        except Exception as e:
            print(f"fetch error: {e}")
            await asyncio.sleep(1)
            continue

        for msg in msgs:
            try:
                order = json.loads(msg.data.decode())
                metadata = msg.metadata
                print(
                    f"processing seq={metadata.sequence.stream} "
                    f"subject={msg.subject} "
                    f"attempt={metadata.num_delivered}"
                )
                await process_order(order)
                await msg.ack()
            except Exception as e:
                print(f"processing failed: {e}")
                await msg.nak(delay=5)
```

## Push Consumer

```python
async def push_consumer(js):
    async def message_handler(msg):
        try:
            order = json.loads(msg.data.decode())
            await process_order(order)
            await msg.ack()
        except Exception as e:
            print(f"error: {e}")
            await msg.nak()

    sub = await js.subscribe(
        "orders.>",
        queue="order-handlers",
        durable="order-handler",
        cb=message_handler,
        manual_ack=True,
        deliver_policy=DeliverPolicy.NEW,
    )
```

## Ordered Consumer (Replay)

```python
async def ordered_replay(js):
    sub = await js.subscribe(
        "orders.>",
        ordered_consumer=True,
    )

    async for msg in sub.messages:
        order = json.loads(msg.data.decode())
        print(f"seq={msg.metadata.sequence.stream} subject={msg.subject}", order)
```

## Stream Management

```python
async def manage_streams(js):
    streams = await js.streams_info()
    for stream in streams:
        print(
            f"stream: {stream.config.name} "
            f"msgs={stream.state.messages} "
            f"bytes={stream.state.bytes}"
        )

    info = await js.stream_info("ORDERS")
    print(f"ORDERS: {info.state.messages} messages")

    await js.purge_stream("ORDERS")
    await js.purge_stream("ORDERS", subject="orders.cancelled")
```

## Graceful Shutdown with Signals

```python
async def run_with_shutdown():
    nc = NATS()
    await nc.connect("nats://localhost:4222")
    js = nc.jetstream()

    psub = await js.pull_subscribe("orders.>", durable="processor")
    shutdown = asyncio.Event()

    def signal_handler():
        print("shutting down...")
        shutdown.set()

    loop = asyncio.get_event_loop()
    loop.add_signal_handler(signal.SIGINT, signal_handler)
    loop.add_signal_handler(signal.SIGTERM, signal_handler)

    while not shutdown.is_set():
        try:
            msgs = await psub.fetch(batch=10, timeout=2)
            for msg in msgs:
                await process_order(json.loads(msg.data))
                await msg.ack()
        except TimeoutError:
            continue

    await nc.drain()
```
