import { Controller, Get, Post, Param, Body } from '@nestjs/common';
import { EventPattern } from '@nestjs/microservices';
import { Cron } from '@nestjs/schedule';

@Controller('invoices')
export class InvoicesController {
  @Get()
  findAll() { return []; }

  @Get(':id')
  findOne(@Param('id') id: string) { return id; }

  @Post(':id/pay')
  @HttpCode(202)
  async pay(@Param('id') id: string, @Body() body: PayDto) {
    return this.svc.pay(id, body);
  }

  @EventPattern('invoice.created')
  onCreated(data: unknown) {}

  @Cron('0 3 * * *')
  nightlyDunning() {}
}
