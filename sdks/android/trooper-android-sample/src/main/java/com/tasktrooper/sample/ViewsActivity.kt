package com.tasktrooper.sample

import android.app.Activity
import android.app.AlertDialog
import android.graphics.Color
import android.graphics.drawable.GradientDrawable
import android.os.Bundle
import android.text.InputType
import android.view.Gravity
import android.view.ViewGroup.LayoutParams.MATCH_PARENT
import android.view.ViewGroup.LayoutParams.WRAP_CONTENT
import android.widget.ArrayAdapter
import android.widget.Button
import android.widget.EditText
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ListView
import android.widget.Switch
import android.widget.TextView
import com.tasktrooper.android.replay.trooperUnmask

/** Classic View screen for session replay: inputs (incl. password), a switch, a dialog and a list. */
class ViewsActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val dp = resources.displayMetrics.density
        fun px(v: Int) = (v * dp).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(px(16), px(32), px(16), px(16))
            setBackgroundColor(Color.parseColor("#F4F5F7"))
        }
        root.addView(TextView(this).apply {
            text = "Classic views"; textSize = 22f; setTextColor(Color.parseColor("#1F2937")); trooperUnmask()
        })
        root.addView(EditText(this).apply { hint = "Name"; tag = "name" }, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))
        root.addView(EditText(this).apply {
            hint = "Password"; inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            trooperUnmask() // must stay masked anyway
        }, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))
        val card = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL
            setPadding(px(12), px(12), px(12), px(12))
            background = GradientDrawable().apply { setColor(Color.parseColor("#4F46E5")); cornerRadius = 12 * dp }
        }
        card.addView(ImageView(this).apply { setImageResource(android.R.drawable.ic_dialog_info) }, LinearLayout.LayoutParams(px(24), px(24)))
        card.addView(TextView(this).apply { text = "Card with secret 1234"; setTextColor(Color.WHITE); setPadding(px(8), 0, 0, 0) })
        root.addView(card, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT).apply { topMargin = px(8) })
        root.addView(Switch(this).apply { text = "Notifications" })
        root.addView(Button(this).apply {
            text = "Show dialog"; tag = "dialog"
            setOnClickListener {
                AlertDialog.Builder(this@ViewsActivity)
                    .setTitle("Confirm order")
                    .setMessage("Card ending 4242 will be charged.")
                    .setView(EditText(this@ViewsActivity).apply { hint = "Coupon" })
                    .setPositiveButton("Pay", null).setNegativeButton("Cancel", null).show()
            }
        })
        root.addView(ListView(this).apply {
            adapter = ArrayAdapter(this@ViewsActivity, android.R.layout.simple_list_item_1, (1..40).map { "Order #$it" })
        }, LinearLayout.LayoutParams(MATCH_PARENT, 0, 1f))
        setContentView(root)
    }
}
