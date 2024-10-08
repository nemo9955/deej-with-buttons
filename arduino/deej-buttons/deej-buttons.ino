#include <Bounce.h>

const int NUM_SLIDERS = 2;
const int analogInputs[NUM_SLIDERS] = {A0, A1};
int analogSliderValues[NUM_SLIDERS];

#define DEBOUNCE_VALUE_MS 5
const int NUM_BUTTONS = 2;
const int analogButtons[NUM_BUTTONS] = {2, 3};
Bounce buttonsBounce[NUM_BUTTONS] = {
    Bounce(analogButtons[0], DEBOUNCE_VALUE_MS),
    Bounce(analogButtons[1], DEBOUNCE_VALUE_MS),
};
int buttonsValues[NUM_BUTTONS];

void setup()
{
    for (int i = 0; i < NUM_SLIDERS; i++)
    {
        pinMode(analogInputs[i], INPUT);
    }

    for (int i = 0; i < NUM_BUTTONS; i++)
    {
        pinMode(analogButtons[i], INPUT);
        // buttonsBounce[i] = Bounce(analogButtons[i], DEBOUNCE_VALUE_MS);
    }

    Serial.begin(9600);
}

void loop()
{
    updateButtonValues();
    updateSliderValues();

    sendSliderValues(); // Actually send data (all the time)
    // printSliderValues(); // For debug
    delay(50);
}

void updateButtonValues()
{
    for (int i = 0; i < NUM_BUTTONS; i++)
    {
        buttonsBounce[i].update();
    }

    for (int i = 0; i < NUM_BUTTONS; i++)
    {
        buttonsValues[i] = !buttonsBounce[i].read();
    }
}

void updateSliderValues()
{
    for (int i = 0; i < NUM_SLIDERS; i++)
    {
        analogSliderValues[i] = analogRead(analogInputs[i]);
    }
}

void sendSliderValues()
{
    String builtSliderString = String("");
    for (int i = 0; i < NUM_SLIDERS; i++)
    {
        builtSliderString += String((int)analogSliderValues[i]);

        if (i < NUM_SLIDERS - 1)
        {
            builtSliderString += String("|");
        }
    }
    Serial.println(builtSliderString);

    String builtButtonString = String("~");
    for (int i = 0; i < NUM_BUTTONS; i++)
    {
        builtButtonString += String((int)buttonsValues[i]);
        builtButtonString += String("~");
    }
    Serial.println(builtButtonString);
}

void printSliderValues()
{
    for (int i = 0; i < NUM_SLIDERS; i++)
    {
        String printedString = String("Slider #") + String(i + 1) + String(": ") + String(analogSliderValues[i]) + String(" mV");
        Serial.write(printedString.c_str());

        if (i < NUM_SLIDERS - 1)
        {
            Serial.write(" | ");
        }
        else
        {
            Serial.write("\n");
        }
    }
}
