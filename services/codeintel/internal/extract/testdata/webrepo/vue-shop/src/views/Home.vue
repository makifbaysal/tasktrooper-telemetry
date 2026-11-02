<template>
  <!-- <router-link to="/in-template">x</router-link> { path: '/in-template-comment', component: X } -->
  <div>{{ title }}</div>
  <RouterView />
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'

const title = ref('Shop')
onMounted(async () => {
  const res = await fetch('https://api.shop.example.com/v1/featured')
  title.value = (await res.json()).title
})
</script>

<style scoped>
div { content: "fetch('https://styles.example.com/x')"; }
</style>
