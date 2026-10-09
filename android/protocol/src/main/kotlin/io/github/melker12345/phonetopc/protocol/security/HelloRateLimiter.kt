package io.github.melker12345.phonetopc.protocol.security

/**
 * Limits failed `hello`s per source address (SECURITY.md §5). Sliding window: once an address
 * has [maxFailures] failures within [windowMillis], it is blocked until the oldest of them ages
 * out. Successful hellos don't reset the count, so guessing can't be interleaved with a
 * valid login from the same address. Thread-safe.
 */
class HelloRateLimiter(
    private val maxFailures: Int = 5,
    private val windowMillis: Long = 60_000,
    private val clock: () -> Long = System::currentTimeMillis,
    /** Bound on tracked addresses so a flood of sources can't grow memory without limit. */
    private val maxAddresses: Int = 1024,
) {
    private val failures = LinkedHashMap<String, ArrayDeque<Long>>()

    @Synchronized
    fun isBlocked(address: String): Boolean {
        val q = failures[address] ?: return false
        prune(q, clock())
        if (q.isEmpty()) failures.remove(address)
        return q.size >= maxFailures
    }

    @Synchronized
    fun recordFailure(address: String) {
        val now = clock()
        val q = failures.getOrPut(address) { ArrayDeque() }
        prune(q, now)
        q.addLast(now)
        while (q.size > maxFailures) q.removeFirst()
        if (failures.size > maxAddresses) {
            // Drop the least recently inserted address.
            failures.keys.firstOrNull()?.let { failures.remove(it) }
        }
    }

    private fun prune(q: ArrayDeque<Long>, now: Long) {
        while (q.isNotEmpty() && now - q.first() >= windowMillis) q.removeFirst()
    }
}
