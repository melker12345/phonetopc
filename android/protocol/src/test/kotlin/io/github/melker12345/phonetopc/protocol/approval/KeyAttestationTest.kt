package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.ProtocolJson
import io.github.melker12345.phonetopc.protocol.conformanceDir
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pins
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test
import java.io.File

/** APPROVAL.md §3.1 against spec/conformance/vectors/attestation.json. */
class KeyAttestationTest {
    private val dir = File(conformanceDir(), "vectors")
    private val vector = Json.parseToJsonElement(File(dir, "attestation.json").readText()).jsonObject

    @Test fun challengeIsTheDigestOfTheTlsKey() {
        val cert = Pins.parsePemCertificate(File(dir, vector["phone_cert"]!!.jsonPrimitive.content).readText())
        val challenge = KeyAttestation.challenge(cert.publicKey.encoded)
        assertEquals(vector["challenge"]!!.jsonPrimitive.content, Base64Url.encode(challenge))
        assertEquals(Pins.of(cert), "sha256/" + Base64Url.encode(challenge))
    }

    @Test fun helloCarriesTheChainLeafFirst() {
        val chain = vector["valid"]!!.jsonArray.map { Base64Url.decode(it.jsonPrimitive.content)!! }
        val key = Base64Url.decode(vector["key"]!!.jsonPrimitive.content)!!
        val json = ProtocolJson.encodeToJsonElement(ApprovalKeyInfo.serializer(), ApprovalKeyInfo.of(key, chain)).jsonObject
        assertEquals(vector["valid"], json["attestation"])
        assertEquals(vector["key"]!!.jsonPrimitive.content, json["key"]!!.jsonPrimitive.content)
    }

    @Test fun noChainNoMember() {
        val key = Base64Url.decode(vector["key"]!!.jsonPrimitive.content)!!
        val json = ProtocolJson.encodeToJsonElement(ApprovalKeyInfo.serializer(), ApprovalKeyInfo.of(key)).jsonObject
        assertFalse(json.containsKey("attestation"))
    }
}
