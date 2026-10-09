package io.github.melker12345.phonetopc.ui.settings

import android.os.Build
import androidx.annotation.ArrayRes
import androidx.annotation.StringRes
import io.github.melker12345.phonetopc.R

/** Vendors whose battery managers need extra steps, per dontkillmyapp.com. */
enum class Vendor(
    @param:StringRes val title: Int,
    @param:ArrayRes val steps: Int,
    val url: String,
) {
    SAMSUNG(R.string.vendor_samsung, R.array.vendor_samsung_steps, "https://dontkillmyapp.com/samsung"),
    XIAOMI(R.string.vendor_xiaomi, R.array.vendor_xiaomi_steps, "https://dontkillmyapp.com/xiaomi"),
    ONEPLUS(R.string.vendor_oneplus, R.array.vendor_oneplus_steps, "https://dontkillmyapp.com/oneplus"),
    OPPO(R.string.vendor_oppo, R.array.vendor_oneplus_steps, "https://dontkillmyapp.com/oppo"),
    REALME(R.string.vendor_realme, R.array.vendor_oneplus_steps, "https://dontkillmyapp.com/realme"),
    HUAWEI(R.string.vendor_huawei, R.array.vendor_huawei_steps, "https://dontkillmyapp.com/huawei"),
    GENERIC(R.string.vendor_generic, R.array.vendor_generic_steps, "https://dontkillmyapp.com/"),
    ;

    companion object {
        fun detect(manufacturer: String = Build.MANUFACTURER): Vendor = when (manufacturer.lowercase().trim()) {
            "samsung" -> SAMSUNG
            "xiaomi", "redmi", "poco" -> XIAOMI
            "oneplus" -> ONEPLUS
            "oppo" -> OPPO
            "realme" -> REALME
            "huawei", "honor" -> HUAWEI
            else -> GENERIC
        }
    }
}
