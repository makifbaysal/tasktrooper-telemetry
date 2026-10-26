plugins {
    `java-library`
}

description = "TaskTrooper Spring Boot 3 starter (no agent): OTel Spring Boot starter + trooper.* configuration"

dependencies {
    api(project(":trooper-otel-core"))
    api(platform(libs.otel.instrumentation.bom))
    api(platform(libs.otel.instrumentation.bom.alpha))
    api(libs.otel.spring.boot.starter)
    implementation("io.opentelemetry.instrumentation:opentelemetry-instrumentation-api")

    compileOnly(platform(libs.spring.boot.bom))
    compileOnly("org.springframework.boot:spring-boot-autoconfigure")
    compileOnly("org.springframework:spring-webmvc")
    compileOnly("jakarta.servlet:jakarta.servlet-api")
    compileOnly("org.springframework.kafka:spring-kafka")
    compileOnly("org.springframework.amqp:spring-rabbit")
    compileOnly("org.springframework.retry:spring-retry")

    testImplementation(platform(libs.spring.boot.bom))
    testImplementation("org.springframework.boot:spring-boot-starter-web")
    testImplementation("org.springframework.boot:spring-boot-starter-webflux")
    testImplementation("org.springframework.boot:spring-boot-starter-test")
    testImplementation("org.springframework.kafka:spring-kafka")
    testImplementation(libs.otel.sdk.testing)
}
