const { Kafka } = require('kafkajs');
const { Worker } = require('bullmq');
const cron = require('node-cron');
const amqp = require('amqplib');
const { Consumer } = require('sqs-consumer');

const kafka = new Kafka({ brokers: ['b:9092'] });
const consumer = kafka.consumer({ groupId: 'payment-events' });

async function run() {
  await consumer.subscribe({ topics: ['payments.captured', 'payments.failed'] });
  await consumer.run({
    eachMessage: async ({ message }) => { await settle(message); },
  });
}

new Worker('emails', async (job) => { await send(job); });

cron.schedule('0 * * * *', rebuildSearchIndex);

async function rabbit(ch) {
  ch.consume('order.shipped', onShipped);
}

Consumer.create({ queueUrl: process.env.REFUNDS_QUEUE_URL, handleMessage: refund });

function rebuildSearchIndex() {}
function onShipped() {}
function refund() {}
fetch('https://hooks.slack.com/services/x');
