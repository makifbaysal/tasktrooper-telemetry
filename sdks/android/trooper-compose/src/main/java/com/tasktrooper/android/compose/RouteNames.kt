package com.tasktrooper.android.compose

/**
 * Converts a Navigation route template into a view name: `{arg}` → `:arg`, optional query
 * arguments dropped, typed (serializable) routes reduced to the simple class name.
 */
fun routeToViewName(route: String?): String? {
    if (route.isNullOrBlank()) return null
    var r = route.substringBefore('?')
    // Type-safe navigation: "com.example.ProductRoute/{id}" → "ProductRoute/:id"
    val firstSegment = r.substringBefore('/')
    if (firstSegment.contains('.')) r = firstSegment.substringAfterLast('.') + r.removePrefix(firstSegment)
    return r.replace(Regex("\\{([^}]+)\\}"), ":$1")
}
