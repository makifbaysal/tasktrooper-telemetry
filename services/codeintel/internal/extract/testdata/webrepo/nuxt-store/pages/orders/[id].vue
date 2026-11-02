<template>
  <div>Order {{ $route.params.id }}</div>
</template>

<script lang="ts">
export default {
  async mounted() {
    await fetch('https://orders.store.example.com/v1/orders')
  },
}
</script>
