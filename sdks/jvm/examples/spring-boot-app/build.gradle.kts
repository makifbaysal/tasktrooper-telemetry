plugins {
    java
}

description = "Example: Spring Boot 3 + trooper-spring-boot-starter (no agent) with a Kafka listener"

dependencies {
    implementation(project(":trooper-spring-boot-starter"))
    implementation(platform(libs.spring.boot.bom))
    implementation("org.springframework.boot:spring-boot-starter-web")
    implementation("org.springframework.kafka:spring-kafka")
}

tasks.register<JavaExec>("run") {
    group = "application"
    classpath = sourceSets.main.get().runtimeClasspath
    mainClass.set("io.tasktrooper.examples.spring.OrdersApplication")
}
