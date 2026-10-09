package io.github.melker12345.phonetopc.protocol

class FakeSensorSource : SensorSource {
    val unavailable = mutableSetOf<String>()
    val latest = mutableMapOf<String, Reading>()
    val demandLog = mutableListOf<Pair<String, Demand?>>()
    val current = mutableMapOf<String, Demand>()
    private var sink: ReadingSink? = null

    override fun describe(): List<SensorInfo> = SensorCatalog.all.map { spec ->
        SensorInfo.of(
            spec,
            available = spec.source !in unavailable,
            minRateHz = 1.0,
            maxRateHz = 200.0,
            defaultRateHz = 50.0,
        )
    }

    override fun setDemand(source: String, demand: Demand?) {
        demandLog += source to demand
        if (demand == null) current.remove(source) else current[source] = demand
    }

    override suspend fun read(source: String): Reading? = latest[source]

    override fun setSink(sink: ReadingSink?) {
        this.sink = sink
    }

    fun emit(reading: Reading) {
        sink?.onReading(reading)
    }

    fun emitVec(source: String, type: String, tsNs: Long, vararg v: Float) =
        emit(Reading(source, type, Values.vec(*v), SensorCatalog[source]!!.unit, tsNs / 1_000_000, tsNs))

    fun emitNumber(source: String, type: String, tsNs: Long, v: Float) =
        emit(Reading(source, type, Values.number(v), SensorCatalog[source]!!.unit, tsNs / 1_000_000, tsNs))
}
