# Redis failover

How to fail over the redis primary when the RedisDown alert fires.

## Steps

1. Confirm the primary is unreachable: `redis-cli -h redis-0 ping`.
2. Promote the replica: `redis-cli -h redis-1 replicaof no one`.
3. Repoint the service selector to the new primary.
