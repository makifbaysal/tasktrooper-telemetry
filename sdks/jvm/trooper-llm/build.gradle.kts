plugins {
    `java-library`
}

description = "TaskTrooper LLM monitoring helpers: TrooperLlm manual GenAI spans, Anthropic Java SDK wrapper, LangChain4j listener (contracts.md §25)"

dependencies {
    // Only the OTel API: works with the javaagent (bridged), the Spring Boot starter and plain SDK setups.
    api(platform(libs.otel.bom))
    api(libs.otel.api)
    // Optional integrations, never required at runtime: classes referencing them load only when used.
    compileOnly(libs.langchain4j.core)

    testImplementation(libs.otel.sdk)
    testImplementation(libs.otel.sdk.testing)
    testImplementation(libs.anthropic.java)
    testImplementation(libs.langchain4j.core)
}
