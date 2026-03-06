# Python Producer Example

```python
import nats

nc = await nats.connect()
js = nc.jetstream()

await js.publish("orders.created", b"123")
```
